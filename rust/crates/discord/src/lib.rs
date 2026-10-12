//! `discord` -- mirrors the Go package `go\internal\discord`.
//!
//! Local Discord IPC pipe transport over `\\.\pipe\discord-ipc-0..9`:
//! handshake, `SET_ACTIVITY`, `READY` gating, reply matching.
//!
//! This file is the module body for `go\internal\discord\pipe.go`: a named-pipe
//! client for `\\.\pipe\discord-ipc-0..9`. Frame = int32 LE opcode + int32 LE
//! length + JSON payload. Opcodes: 0 handshake, 1 frame, 2 close, 3 ping,
//! 4 pong. The client sends the handshake, a read-pump thread answers PING with
//! PONG and surfaces READY/dispatch frames, and any transport error triggers a
//! reconnect after `Config::retry_delay` (5000 ms default), with the contract's
//! `session dropped: <err>` line emitted through `Config::on_drop`.
//!
//! Discord answers every command with the SAME nonce. `send` records each
//! outstanding nonce in a nonce-keyed set and the read pump matches an incoming
//! frame against it; a matched frame is surfaced through `Config::on_reply` (an
//! `evt:"ERROR"` rejection carries Discord's own `data.code` and `data.message`).
//! Commands are held back until Discord's READY dispatch (`Error::NotReady`
//! before it), because Discord discards commands sent earlier; READY also fires
//! `Config::on_connect` and bumps the generation. Every write is bounded by a
//! deadline, because the write mutex is held across it: a peer that accepted the
//! connection but stopped draining would otherwise stall the poll loop forever.
//! Nothing on this path is swallowed: the caller logs every reply, every send
//! error, a `push ack timeout` when a command gets no reply, and a write expiry.
//!
//! The Win32 plumbing -- the `FILE_FLAG_OVERLAPPED` contract, one event per
//! operation, deadline cancellation -- lives in `win.rs`.

pub mod activity;
pub mod fakepipe;

#[cfg(test)]
mod activity_test;
#[cfg(test)]
mod pipe_test;

mod win;

use std::collections::HashMap;
use std::io;
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

use crate::activity::{new_activity, new_nonce, pid, ActivityPayload, Command, SetActivityArgs};
use crate::win::{NamedPipe, ReadBytes, WriteBytes};

pub use crate::activity::{wire_json, Activity, ActivityButton};
pub use crate::fakepipe::{FakeConn, FakeServer};

/// The frame's leading 32-bit little-endian opcode.
pub type Opcode = i32;

/// Discord IPC opcodes.
pub const OP_HANDSHAKE: Opcode = 0;
/// Discord IPC opcodes.
pub const OP_FRAME: Opcode = 1;
/// Discord IPC opcodes.
pub const OP_CLOSE: Opcode = 2;
/// Discord IPC opcodes.
pub const OP_PING: Opcode = 3;
/// Discord IPC opcodes.
pub const OP_PONG: Opcode = 4;

/// The Discord IPC namespace; suffix 0..9 is appended.
pub const DEFAULT_PIPE_BASE: &str = r"\\.\pipe\discord-ipc-";

/// The number of Discord IPC sockets probed per attempt.
const PIPE_SUFFIXES: i32 = 10;

/// Bounds a single payload read, so a corrupt length cannot make the client
/// allocate unbounded memory.
const MAX_FRAME_BYTES: i32 = 1 << 20;

/// How long a pushed command waits for Discord's reply before the client reports
/// the write path as unproven (`push ack timeout`): a write that left the process
/// but was starved/dead gets said out loud, not swallowed.
const ACK_TIMEOUT: Duration = Duration::from_secs(2);

/// One transport failure.
#[derive(Debug)]
pub enum Error {
    /// A frame was pushed before the pipe was up (or after it dropped). The
    /// caller keeps polling; the supervisor reconnects.
    NotConnected,
    /// A command was pushed after the handshake but before Discord's READY
    /// dispatch. Discord discards commands sent before READY, so nothing is
    /// written until the session is ready.
    NotReady,
    /// A bounded write expired mid-frame: the byte stream is truncated, so the
    /// connection was aborted and the supervisor will reconnect.
    WriteTimeout {
        after: Duration,
        source: io::Error,
    },
    /// A transport error (dial, handshake, read pump, or a closed peer).
    Io(io::Error),
    /// A command or handshake that could not be serialized (Go returns the
    /// `encoding/json` error as-is).
    Json(serde_json::Error),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::NotConnected => write!(f, "discord: not connected"),
            Error::NotReady => write!(f, "discord: not ready"),
            Error::WriteTimeout { after, source } => {
                write!(f, "discord: write timeout after {after:?}: {source}")
            }
            Error::Io(e) => std::fmt::Display::fmt(e, f),
            Error::Json(e) => std::fmt::Display::fmt(e, f),
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Error::WriteTimeout { source, .. } => Some(source),
            Error::Io(e) => Some(e),
            Error::Json(e) => Some(e),
            Error::NotConnected | Error::NotReady => None,
        }
    }
}

impl From<io::Error> for Error {
    fn from(e: io::Error) -> Error {
        Error::Io(e)
    }
}

/// Callback invoked after a successful handshake (the generation is bumped).
pub type ConnectFn = Box<dyn Fn() + Send + Sync>;
/// Callback invoked with every raw dispatch payload.
pub type FrameFn = Box<dyn Fn(&[u8]) + Send + Sync>;
/// Callback invoked with a frame whose nonce matched an outstanding command.
pub type ReplyFn = Box<dyn Fn(Reply) + Send + Sync>;
/// Callback invoked when an outstanding command gets no reply in `ACK_TIMEOUT`.
pub type AckTimeoutFn = Box<dyn Fn(&str) + Send + Sync>;
/// Callback invoked when the transport drops, before the reconnect delay.
pub type DropFn = Box<dyn Fn(&Error) + Send + Sync>;

/// The one-time client hello: `{"v":1,"client_id":"<id>"}`.
#[derive(Debug, Serialize)]
struct Handshake<'a> {
    v: i32,
    client_id: &'a str,
}

/// Configures one `Client`. Only `client_id` is required; the rest fall back to
/// the contract defaults (retry delay 5000 ms) or the fixed Discord namespace,
/// applied by `Client::new` -- so the zero value is the empty config, as in Go.
#[derive(Default)]
pub struct Config {
    pub client_id: String,
    /// Default `DEFAULT_PIPE_BASE`.
    pub pipe_base: String,
    /// Default 5s; the contract's reconnect delay.
    pub retry_delay: Duration,
    pub on_connect: Option<ConnectFn>,
    pub on_frame: Option<FrameFn>,
    pub on_reply: Option<ReplyFn>,
    pub on_ack_timeout: Option<AckTimeoutFn>,
    pub on_drop: Option<DropFn>,
}

/// One server frame matched to an outstanding command by nonce. `error` is true
/// for an `evt:"ERROR"` rejection; `code`/`message` then carry Discord's own
/// `data.code` / `data.message` (the client id is redacted before `message` is
/// exposed).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Reply {
    pub nonce: String,
    pub evt: String,
    pub code: i32,
    pub message: String,
    pub error: bool,
}

/// The connection state guarded by one mutex.
struct State {
    conn: Option<Arc<NamedPipe>>,
    ready: bool,
    shut: bool,
}

/// Everything shared between the client handle and its pump thread.
struct Inner {
    cfg: Config,
    state: Mutex<State>,
    /// Pairs with `state`; wakes the reconnect sleep on close.
    retry: Condvar,
    /// Serializes frame writes.
    wmu: Mutex<()>,
    /// In-flight commands, keyed by nonce.
    pending: Mutex<HashMap<String, ()>>,
    generation: AtomicI64,
}

/// One reconnect-aware IPC client. `start` launches the supervisor; all writes
/// are serialized so a PONG from the read pump cannot interleave with a
/// SET_ACTIVITY frame.
pub struct Client {
    inner: Arc<Inner>,
    /// Bounds every frame write (default `ACK_TIMEOUT`). The write mutex is held
    /// across a write, so without this a non-draining peer stalls the poll loop.
    /// Test seam: set it before `start`.
    write_timeout: Duration,
}

impl Client {
    /// Returns a client; it does not connect until `start`.
    pub fn new(mut cfg: Config) -> Client {
        if cfg.pipe_base.is_empty() {
            cfg.pipe_base = DEFAULT_PIPE_BASE.to_string();
        }
        if cfg.retry_delay.is_zero() {
            cfg.retry_delay = Duration::from_secs(5);
        }
        Client {
            inner: Arc::new(Inner {
                cfg,
                state: Mutex::new(State {
                    conn: None,
                    ready: false,
                    shut: false,
                }),
                retry: Condvar::new(),
                wmu: Mutex::new(()),
                pending: Mutex::new(HashMap::new()),
                generation: AtomicI64::new(0),
            }),
            write_timeout: ACK_TIMEOUT,
        }
    }

    /// Launches the connect/pump/reconnect supervisor on its own thread.
    pub fn start(&self) {
        let inner = Arc::clone(&self.inner);
        let write_timeout = self.write_timeout;
        drop(thread::spawn(move || supervise(inner, write_timeout)));
    }

    /// Stops the supervisor and closes the pipe. Safe to call twice.
    pub fn close(&self) -> io::Result<()> {
        let conn = {
            let mut st = self.inner.state.lock().expect("client state");
            if st.shut {
                return Ok(());
            }
            st.shut = true;
            st.ready = false;
            st.conn.take()
        };
        self.inner.retry.notify_all();
        self.inner.clear_pending();
        if let Some(c) = conn {
            c.close();
        }
        Ok(())
    }

    /// Increments on every successful handshake. The poll loop watches it to
    /// detect a reconnect and re-emit the current card (contract section 4:
    /// "first poll after connect").
    pub fn generation(&self) -> i64 {
        self.inner.generation.load(Ordering::SeqCst)
    }

    /// Sends one SET_ACTIVITY command (`activity.rs`).
    pub fn set_activity(&self, a: Activity) -> Result<(), Error> {
        self.send("SET_ACTIVITY", Some(new_activity(&a)))
    }

    /// Sends SET_ACTIVITY with a null activity.
    pub fn clear_activity(&self) -> Result<(), Error> {
        self.send("SET_ACTIVITY", None)
    }

    /// Marshals and writes one frame. The connection pointer is snapshotted
    /// under the state lock (`connect_and_pump` and `close` replace it there),
    /// then the write itself is serialized by the write mutex; a reply cannot
    /// race the pointer read.
    fn send(&self, cmd: &'static str, activity: Option<ActivityPayload>) -> Result<(), Error> {
        let cmd = Command {
            cmd,
            args: SetActivityArgs {
                pid: pid(),
                activity,
            },
            nonce: new_nonce(),
        };
        let payload = serde_json::to_vec(&cmd).map_err(Error::Json)?;
        let (conn, ready) = {
            let st = self.inner.state.lock().expect("client state");
            (st.conn.clone(), st.ready)
        };
        let Some(conn) = conn else {
            return Err(Error::NotConnected);
        };
        if !ready {
            return Err(Error::NotReady);
        }
        // Record the outstanding nonce before the bytes leave, so a reply that
        // arrives immediately still matches, and arm the ack timeout.
        self.inner.begin_pending(&cmd.nonce);
        let _wmu = self.inner.wmu.lock().expect("write mutex");
        // The write mutex is held across the write, so bound it: a peer that
        // accepted the connection but stopped draining must not stall this send
        // (and every later one) forever. On expiry, drop the connection so the
        // read pump unblocks and the supervisor reconnects.
        conn.set_write_deadline(Some(Instant::now() + self.write_timeout));
        let res = write_frame(&*conn, OP_FRAME, &payload);
        conn.set_write_deadline(None);
        match res {
            Err(e) if e.kind() == io::ErrorKind::TimedOut => {
                self.abort(&conn);
                Err(Error::WriteTimeout {
                    after: self.write_timeout,
                    source: e,
                })
            }
            other => other.map_err(Error::Io),
        }
    }

    /// Drops the current connection so the blocked read pump unblocks and the
    /// supervisor reconnects. Used when a write expired mid-frame: the byte
    /// stream is now truncated, so the connection is not reusable.
    fn abort(&self, conn: &Arc<NamedPipe>) {
        {
            let mut st = self.inner.state.lock().expect("client state");
            if st.conn.as_ref().is_some_and(|cur| Arc::ptr_eq(cur, conn)) {
                st.conn = None;
                st.ready = false;
            }
        }
        conn.close();
    }
}

impl Inner {
    fn closed(&self) -> bool {
        self.state.lock().expect("client state").shut
    }

    /// Records one outstanding command nonce and arms its ack timeout: if the
    /// nonce is still outstanding after `ACK_TIMEOUT`, the write path is reported
    /// as unproven rather than silently trusted. Commands are keyed by nonce, not
    /// held in one slot, so two overlapping sends (clear-on-exit racing the poll
    /// loop) each keep their own verdict instead of the later one overwriting the
    /// earlier.
    fn begin_pending(self: &Arc<Inner>, nonce: &str) {
        self.pending
            .lock()
            .expect("pending")
            .insert(nonce.to_string(), ());
        if self.cfg.on_ack_timeout.is_none() {
            return;
        }
        let inner = Arc::clone(self);
        let nonce = nonce.to_string();
        drop(thread::spawn(move || {
            thread::sleep(ACK_TIMEOUT);
            inner.ack_expired(&nonce);
        }));
    }

    /// Reports a command still unanswered after `ACK_TIMEOUT`, once.
    fn ack_expired(&self, nonce: &str) {
        let was_pending = self
            .pending
            .lock()
            .expect("pending")
            .remove(nonce)
            .is_some();
        if was_pending && !self.closed() && let Some(cb) = &self.cfg.on_ack_timeout {
            cb(nonce);
        }
    }

    /// Clears and reports whether `nonce` is an outstanding command, and disarms
    /// its ack timeout.
    fn take_pending(&self, nonce: &str) -> bool {
        self.pending
            .lock()
            .expect("pending")
            .remove(nonce)
            .is_some()
    }

    /// Drops every outstanding command (on drop/close).
    fn clear_pending(&self) {
        self.pending.lock().expect("pending").clear();
    }

    /// Sleeps `d`, or returns early (true) when the client is shut.
    fn retry_wait(&self, d: Duration) -> bool {
        let shut = self.state.lock().expect("client state");
        if shut.shut {
            return true;
        }
        let (guard, _) = self.retry.wait_timeout(shut, d).expect("client state");
        guard.shut
    }
}

/// Connects, pumps until the transport drops, then retries after the retry
/// delay.
fn supervise(inner: Arc<Inner>, write_timeout: Duration) {
    loop {
        let err = connect_and_pump(&inner, write_timeout);
        if inner.closed() {
            return;
        }
        if let Err(e) = err && let Some(cb) = &inner.cfg.on_drop {
            cb(&e);
        }
        if inner.retry_wait(inner.cfg.retry_delay) {
            return;
        }
    }
}

/// Dials one suffix, sends the handshake, and reads frames until an error. The
/// connection is published only after the handshake is out.
fn connect_and_pump(inner: &Arc<Inner>, write_timeout: Duration) -> Result<(), Error> {
    let conn = Arc::new(dial(inner).map_err(Error::Io)?);

    let handshake = serde_json::to_vec(&Handshake {
        v: 1,
        client_id: &inner.cfg.client_id,
    })
    .map_err(Error::Json)?;
    {
        let _wmu = inner.wmu.lock().expect("write mutex");
        write_frame_bounded(&conn, OP_HANDSHAKE, &handshake, write_timeout)?;
    }

    {
        let mut st = inner.state.lock().expect("client state");
        st.conn = Some(Arc::clone(&conn));
        st.ready = false;
    }
    // The connection is published but NOT usable yet: Discord discards commands
    // sent before its READY dispatch, so `send` returns `Error::NotReady` until
    // READY arrives (see `dispatch_frame`).
    if inner.closed() {
        // `close` could not reach this connection (it was not published yet), so
        // honour the shutdown here instead of parking the pump on a dead pipe.
        let mut st = inner.state.lock().expect("client state");
        st.conn = None;
        st.ready = false;
        drop(st);
        conn.close();
        return Err(Error::NotConnected);
    }

    let result = pump(inner, &conn, write_timeout);

    // The connection is gone: no reply can arrive.
    {
        let mut st = inner.state.lock().expect("client state");
        st.conn = None;
        st.ready = false;
    }
    inner.clear_pending();
    conn.close();
    result
}

/// Probes suffix 0..9 and returns the first pipe that opens. A failure on every
/// suffix is reported so the supervisor retries.
fn dial(inner: &Arc<Inner>) -> io::Result<NamedPipe> {
    let mut last_err: Option<io::Error> = None;
    for i in 0..PIPE_SUFFIXES {
        let name = format!("{}{}", inner.cfg.pipe_base, i);
        match NamedPipe::open(&name) {
            Ok(pipe) => return Ok(pipe),
            Err(e) => last_err = Some(e),
        }
    }
    let last = last_err
        .unwrap_or_else(|| io::Error::new(io::ErrorKind::NotFound, "no pipe suffix probed"));
    Err(io::Error::new(
        last.kind(),
        format!("no discord-ipc pipe 0..{}: {last}", PIPE_SUFFIXES - 1),
    ))
}

/// Reads frames until the peer closes or the transport errors.
fn pump(inner: &Arc<Inner>, conn: &NamedPipe, write_timeout: Duration) -> Result<(), Error> {
    loop {
        let (op, payload) = read_frame(conn).map_err(Error::Io)?;
        match op {
            OP_PING => {
                let _wmu = inner.wmu.lock().expect("write mutex");
                write_frame_bounded(conn, OP_PONG, &payload, write_timeout)?;
            }
            OP_FRAME => dispatch_frame(inner, &payload),
            OP_CLOSE => {
                return Err(Error::Io(io::Error::new(
                    io::ErrorKind::ConnectionAborted,
                    "closed by peer",
                )));
            }
            // Unsolicited PONG: nothing to do. The client sends the handshake, so
            // a server-side one is unexpected. An unknown opcode is ignored to
            // keep the stream in sync.
            _ => {}
        }
    }
}

/// One inbound dispatch frame; an absent field stays `None` (Go's `*string`), and
/// a payload that does not decode leaves every field empty (Go ignores the
/// unmarshal error).
#[derive(Debug, Default, Deserialize)]
struct InFrame {
    evt: Option<String>,
    nonce: Option<String>,
    data: Option<serde_json::Value>,
}

/// Discord's own rejection body.
#[derive(Debug, Deserialize)]
struct ReplyData {
    code: i32,
    message: String,
}

/// Handles one `OP_FRAME` payload: it completes the handshake on Discord's READY
/// dispatch (bumping the generation and firing `on_connect`), then surfaces a
/// matched command reply (`on_reply`) and finally the raw frame (`on_frame`). A
/// frame with no matching nonce that is not READY is not a reply.
fn dispatch_frame(inner: &Arc<Inner>, payload: &[u8]) {
    let m: InFrame = serde_json::from_slice(payload).unwrap_or_default();

    // READY completes the handshake. Only now may commands be sent; the
    // generation bump tells the poll loop to re-emit the current card.
    if m.evt.as_deref() == Some("READY") {
        let already = {
            let mut st = inner.state.lock().expect("client state");
            let already = st.ready;
            st.ready = true;
            already
        };
        if !already {
            inner.generation.fetch_add(1, Ordering::SeqCst);
            if let Some(cb) = &inner.cfg.on_connect {
                cb();
            }
        }
    }

    let nonce = m.nonce.as_deref().unwrap_or("");
    if inner.cfg.on_reply.is_some() && !nonce.is_empty() && inner.take_pending(nonce) {
        let mut r = Reply {
            nonce: nonce.to_string(),
            ..Reply::default()
        };
        if let Some(evt) = &m.evt {
            r.evt = evt.clone();
            r.error = evt == "ERROR";
        }
        if r.error
            && let Some(data) = &m.data
            && let Ok(d) = serde_json::from_value::<ReplyData>(data.clone())
        {
            r.code = d.code;
            r.message = redact_client_id(&d.message, &inner.cfg.client_id);
        }
        if let Some(cb) = &inner.cfg.on_reply {
            cb(r);
        }
    }
    if let Some(cb) = &inner.cfg.on_frame {
        cb(payload);
    }
}

/// Removes the resolved client id from a message before it can be logged; the id
/// VALUE is never written (M1 secret rule).
fn redact_client_id(s: &str, client_id: &str) -> String {
    if s.is_empty() || client_id.is_empty() {
        return s.to_string();
    }
    s.replace(client_id, "<redacted>")
}

/// Writes one frame with a deadline, so a blocked write cannot hold the write
/// mutex indefinitely. The handle is overlapped, so the deadline cancels a stuck
/// write; on a non-pollable handle it would proceed unbounded.
fn write_frame_bounded(
    conn: &NamedPipe,
    op: Opcode,
    payload: &[u8],
    timeout: Duration,
) -> Result<(), Error> {
    conn.set_write_deadline(Some(Instant::now() + timeout));
    let res = write_frame(conn, op, payload);
    conn.set_write_deadline(None);
    res.map_err(Error::Io)
}

/// Writes the 8-byte header and payload as one logical frame. Go issues the
/// header and the payload as two writes; on a byte-mode pipe the resulting stream
/// is identical, and a single bounded write keeps the frame under one deadline.
fn write_frame<W: WriteBytes + ?Sized>(w: &W, op: Opcode, payload: &[u8]) -> io::Result<()> {
    let len = i32::try_from(payload.len()).map_err(|_| {
        io::Error::new(io::ErrorKind::InvalidInput, "frame payload too large")
    })?;
    let mut frame = Vec::with_capacity(8 + payload.len());
    frame.extend_from_slice(&op.to_le_bytes());
    frame.extend_from_slice(&len.to_le_bytes());
    frame.extend_from_slice(payload);
    write_all(w, &frame)
}

/// Reads one header and its payload.
fn read_frame<R: ReadBytes + ?Sized>(r: &R) -> io::Result<(Opcode, Vec<u8>)> {
    let mut hdr = [0u8; 8];
    read_full(r, &mut hdr)?;
    let op = Opcode::from_le_bytes([hdr[0], hdr[1], hdr[2], hdr[3]]);
    let n = i32::from_le_bytes([hdr[4], hdr[5], hdr[6], hdr[7]]);
    if !(0..=MAX_FRAME_BYTES).contains(&n) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("bad frame length {n}"),
        ));
    }
    let mut payload = vec![0u8; n as usize];
    read_full(r, &mut payload)?;
    Ok((op, payload))
}

/// Writes every byte of `buf`: a short write is retried, and a zero-length write
/// is an error (Go's `io.Writer` contract).
fn write_all<W: WriteBytes + ?Sized>(w: &W, buf: &[u8]) -> io::Result<()> {
    let mut off = 0;
    while off < buf.len() {
        let n = w.write_some(&buf[off..])?;
        if n == 0 {
            return Err(io::Error::new(
                io::ErrorKind::WriteZero,
                "named pipe write returned 0 bytes",
            ));
        }
        off += n;
    }
    Ok(())
}

/// Fills `buf` completely (`io.ReadFull`).
fn read_full<R: ReadBytes + ?Sized>(r: &R, buf: &mut [u8]) -> io::Result<()> {
    let mut off = 0;
    while off < buf.len() {
        let n = r.read_some(&mut buf[off..])?;
        if n == 0 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "named pipe closed",
            ));
        }
        off += n;
    }
    Ok(())
}

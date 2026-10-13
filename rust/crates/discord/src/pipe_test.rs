//! `discord::pipe_test` -- mirrors `go\internal\discord\pipe_test.go`: transport
//! tests against the in-process fake pipe (handshake shape, suffix fallback,
//! PING -> PONG, reconnect after the server drops, bounded writes, overlapping
//! sends). No test connects to the real `\\.\pipe\discord-ipc-*` namespace.

use std::io;
use std::ops::Deref;
use std::sync::mpsc;
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

use serde_json::Value;

use crate::activity::Activity;
use crate::fakepipe::{FakeConn, FakeServer};
use crate::{Client, Config, Error, Reply, OP_FRAME, OP_HANDSHAKE, OP_PING, OP_PONG};

/// Discord's READY dispatch: the client refuses to send commands before it.
pub(crate) const READY: &[u8] = br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#;

/// Closes the client when the test ends (Go's `t.Cleanup`).
pub(crate) struct ClientGuard(pub(crate) Client);

impl Deref for ClientGuard {
    type Target = Client;

    fn deref(&self) -> &Client {
        &self.0
    }
}

impl Drop for ClientGuard {
    fn drop(&mut self) {
        let _ = self.0.close();
    }
}

/// Accepts one connection on a fresh pipe instance, off the test thread.
pub(crate) fn spawn_accept(
    srv: Arc<FakeServer>,
) -> (mpsc::Receiver<FakeConn>, mpsc::Receiver<io::Error>) {
    let (tx, rx) = mpsc::channel();
    let (etx, erx) = mpsc::channel();
    drop(thread::spawn(move || match srv.accept() {
        Ok(sc) => {
            let _ = tx.send(sc);
        }
        Err(e) => {
            let _ = etx.send(e);
        }
    }));
    (rx, erx)
}

/// Waits for an accepted connection (Go's `select` on accept / acceptErr / 5s).
pub(crate) fn accept_one(
    rx: &mpsc::Receiver<FakeConn>,
    erx: &mpsc::Receiver<io::Error>,
    what: &str,
) -> FakeConn {
    match rx.recv_timeout(Duration::from_secs(5)) {
        Ok(sc) => sc,
        Err(_) => match erx.try_recv() {
            Ok(e) => panic!("fake server accept: {e}"),
            Err(_) => panic!("{what}"),
        },
    }
}

/// connectPair: starts a FakeServer on `suffix` and a Client pointed at it, waits
/// for the connection, consumes and returns the client's handshake frame.
pub(crate) fn connect_pair(suffix: i32, mut cfg: Config) -> (ClientGuard, FakeConn, Vec<u8>) {
    let srv = FakeServer::new(suffix);
    let base = srv.base.clone();
    let (rx, erx) = spawn_accept(Arc::new(srv));

    if cfg.client_id.is_empty() {
        cfg.client_id = "123456789012345678".to_string();
    }
    if cfg.pipe_base.is_empty() {
        cfg.pipe_base = base;
    }
    if cfg.retry_delay.is_zero() {
        cfg.retry_delay = Duration::from_millis(30);
    }
    let c = ClientGuard(Client::new(cfg));
    c.start();

    let sc = accept_one(&rx, &erx, "client did not connect to the fake pipe");

    let (op, hs) = sc
        .read_frame()
        .unwrap_or_else(|e| panic!("read handshake: {e}"));
    assert_eq!(op, OP_HANDSHAKE, "first frame opcode");
    sc.write_frame(OP_FRAME, READY)
        .unwrap_or_else(|e| panic!("write READY: {e}"));
    wait_ready(&c, 1);
    (c, sc, hs)
}

/// Blocks until the client's generation reaches `want`.
pub(crate) fn wait_ready(c: &Client, want: i64) {
    let deadline = Instant::now() + Duration::from_secs(3);
    while c.generation() < want {
        if Instant::now() > deadline {
            panic!(
                "client never became ready (gen={}, want >={want})",
                c.generation()
            );
        }
        thread::sleep(Duration::from_millis(2));
    }
}

/// Decodes the nonce from a client frame payload.
pub(crate) fn nonce_of(payload: &[u8]) -> String {
    let m: Value = serde_json::from_slice(payload)
        .unwrap_or_else(|e| panic!("decode frame {}: {e}", String::from_utf8_lossy(payload)));
    let nonce = m.get("nonce").and_then(Value::as_str).unwrap_or("");
    if nonce.is_empty() {
        panic!(
            "frame carries no nonce: {}",
            String::from_utf8_lossy(payload)
        );
    }
    nonce.to_string()
}

#[test]
fn test_handshake_payload() {
    const ID: &str = "987654321098765432";
    // server on suffix 3: 0..2 must fail first
    let (_c, sc, hs) = connect_pair(
        3,
        Config {
            client_id: ID.to_string(),
            ..Config::default()
        },
    );

    let want = r#"{"v":1,"client_id":"987654321098765432"}"#;
    assert_eq!(String::from_utf8_lossy(&hs).as_ref(), want, "handshake");
    sc.close();
}

#[test]
fn test_ping_pong() {
    let (_c, sc, _) = connect_pair(0, Config::default());

    sc.write_frame(OP_PING, b"ping-payload")
        .unwrap_or_else(|e| panic!("write ping: {e}"));
    let (op, payload) = sc.read_frame().unwrap_or_else(|e| panic!("read pong: {e}"));
    assert_eq!(op, OP_PONG, "reply opcode");
    assert_eq!(payload, b"ping-payload", "pong payload");
    sc.close();
}

#[test]
fn test_reconnect_after_drop() {
    let srv = FakeServer::new(0);
    let base = srv.base.clone();
    let (tx, rx) = mpsc::channel();
    drop(thread::spawn(move || {
        for _ in 0..2 {
            let Ok(sc) = srv.accept() else { return };
            let Ok((_, p)) = sc.read_frame() else {
                sc.close();
                return;
            };
            if tx.send(p).is_err() {
                sc.close();
                return;
            }
            sc.close(); // drop so the client must reconnect
        }
    }));

    let (dtx, drx) = mpsc::channel();
    let c = ClientGuard(Client::new(Config {
        client_id: "424242424242424242".to_string(),
        pipe_base: base,
        retry_delay: Duration::from_millis(40),
        on_drop: Some(Box::new(move |_e: &Error| {
            let _ = dtx.send(());
        })),
        ..Config::default()
    }));
    c.start();

    const WANT: &str = r#"{"v":1,"client_id":"424242424242424242"}"#;
    for i in 0..2 {
        let hs = rx
            .recv_timeout(Duration::from_secs(5))
            .unwrap_or_else(|_| panic!("timed out waiting for handshake {i} (reconnect failed)"));
        assert_eq!(String::from_utf8_lossy(&hs).as_ref(), WANT, "handshake {i}");
    }
    drx.recv_timeout(Duration::from_secs(5))
        .unwrap_or_else(|_| panic!("OnDrop was not called after the server closed the pipe"));
}

/// Proves the reviewer's MEDIUM finding is closed: a peer that accepts the
/// connection, completes READY, then stops draining must not stall the client.
/// The send returns once the write deadline expires, the client drops the
/// truncated connection and reconnects, and a later push is served on the fresh
/// connection.
#[test]
fn test_stalled_peer_write_is_bounded() {
    let srv = Arc::new(FakeServer {
        buffer_bytes: 256, // small enough that a normal SET_ACTIVITY blocks
        ..FakeServer::new(0)
    });
    let base = srv.base.clone();
    let (rx, erx) = spawn_accept(Arc::clone(&srv));

    let (rtx, rrx) = mpsc::channel();
    let mut client = Client::new(Config {
        client_id: "123456789012345678".to_string(),
        pipe_base: base,
        retry_delay: Duration::from_millis(30),
        on_reply: Some(Box::new(move |r: Reply| {
            let _ = rtx.send(r);
        })),
        ..Config::default()
    });
    // test-only; the production default is ACK_TIMEOUT
    client.write_timeout = Duration::from_millis(200);
    let c = Arc::new(ClientGuard(client));
    c.start();

    let sc = accept_one(&rx, &erx, "client did not connect to the fake pipe");
    let (op, _) = sc.read_frame().unwrap_or_else(|e| panic!("handshake: {e}"));
    assert_eq!(op, OP_HANDSHAKE, "handshake");
    sc.write_frame(OP_FRAME, READY)
        .unwrap_or_else(|e| panic!("write READY: {e}"));
    wait_ready(&c, 1);

    // The server now stops reading. A payload larger than the pipe buffer blocks
    // in the write unless the deadline bounds it.
    let (stx, srx) = mpsc::channel();
    let start = Instant::now();
    {
        let c = Arc::clone(&c);
        drop(thread::spawn(move || {
            let _ = stx.send(c.set_activity(Activity {
                details: "x".repeat(4096),
                state: "S1 E1".to_string(),
                ..Activity::default()
            }));
        }));
    }
    match srx.recv_timeout(Duration::from_secs(5)) {
        Ok(Ok(())) => panic!("SetActivity to a stalled peer returned Ok, want a write error"),
        Ok(Err(_)) => {
            let elapsed = start.elapsed();
            assert!(
                elapsed <= Duration::from_secs(3),
                "SetActivity stalled {elapsed:?}; the write is not bounded"
            );
        }
        Err(_) => panic!("SetActivity never returned; the write is not bounded"),
    }

    // The client must reconnect on a fresh instance; the new server drains and a
    // later push must be served.
    let (rx2, erx2) = spawn_accept(Arc::clone(&srv));
    let sc2 = accept_one(
        &rx2,
        &erx2,
        "client did not reconnect after the bounded write",
    );
    let (op, _) = sc2
        .read_frame()
        .unwrap_or_else(|e| panic!("reconnect handshake: {e}"));
    assert_eq!(op, OP_HANDSHAKE, "reconnect handshake");
    sc2.write_frame(OP_FRAME, READY)
        .unwrap_or_else(|e| panic!("write READY (reconnect): {e}"));
    wait_ready(&c, 2);

    c.set_activity(Activity {
        details: "Show".to_string(),
        state: "S1 E2".to_string(),
        ..Activity::default()
    })
    .unwrap_or_else(|e| panic!("later push after reconnect: {e}"));
    let (op, frame) = sc2
        .read_frame()
        .unwrap_or_else(|e| panic!("read later push: {e}"));
    assert_eq!(op, OP_FRAME, "later push opcode");
    let want = nonce_of(&frame);
    sc2.write_frame(
        OP_FRAME,
        format!(r#"{{"cmd":"SET_ACTIVITY","nonce":"{want}"}}"#).as_bytes(),
    )
    .unwrap_or_else(|e| panic!("write later ack: {e}"));
    match rrx.recv_timeout(Duration::from_secs(3)) {
        Ok(r) => assert_eq!(r.nonce, want, "later push ack nonce"),
        Err(_) => panic!("later push was not served after the bounded write"),
    }
    sc.close();
    sc2.close();
}

/// Proves the reviewer's LOW finding is closed: two sends that overlap
/// (clear-on-exit racing the poll loop) each keep their own verdict; the earlier
/// one is not silently overwritten by the later.
#[test]
fn test_overlapping_sends_both_accounted() {
    let (rtx, rrx) = mpsc::channel();
    let (c, sc, _) = connect_pair(
        0,
        Config {
            on_reply: Some(Box::new(move |r: Reply| {
                let _ = rtx.send(r.nonce);
            })),
            ..Config::default()
        },
    );
    let c = Arc::new(c);

    let (dtx, drx) = mpsc::channel();
    for _ in 0..2 {
        let c = Arc::clone(&c);
        let dtx = dtx.clone();
        drop(thread::spawn(move || {
            let r = c.set_activity(Activity {
                details: "Show".to_string(),
                state: "S1 E2".to_string(),
                ..Activity::default()
            });
            let _ = dtx.send(r);
        }));
    }
    drop(dtx);

    // Read both frames: both sends are now in flight (both begin_pending calls
    // happened before either reply can be written).
    let mut nonces = Vec::new();
    for i in 0..2 {
        let (op, frame) = sc
            .read_frame()
            .unwrap_or_else(|e| panic!("read frame {i}: {e}"));
        assert_eq!(op, OP_FRAME, "frame {i}");
        nonces.push(nonce_of(&frame));
    }
    for r in drx.iter() {
        r.unwrap_or_else(|e| panic!("SetActivity: {e}"));
    }

    for n in &nonces {
        sc.write_frame(
            OP_FRAME,
            format!(r#"{{"cmd":"SET_ACTIVITY","nonce":"{n}"}}"#).as_bytes(),
        )
        .unwrap_or_else(|e| panic!("reply nonce {n}: {e}"));
    }

    let mut got = Vec::new();
    for i in 0..2 {
        match rrx.recv_timeout(Duration::from_secs(3)) {
            Ok(n) => got.push(n),
            Err(_) => panic!("only {i} of 2 verdicts accounted for; got {got:?} want {nonces:?}"),
        }
    }
    for n in &nonces {
        assert!(
            got.contains(n),
            "verdict for nonce {n} was dropped; accounted: {got:?}"
        );
    }
    sc.close();
}

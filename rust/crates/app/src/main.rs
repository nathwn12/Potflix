//! Potflix tray application -- mirrors the Go root package `main` (`go\main.go`).
//!
//! Console-less by design: `#![windows_subsystem = "windows"]` is the equivalent
//! of Go's `-ldflags "-H=windowsgui"`, so `--selftest` output only reaches a
//! parent that redirects stdout -- exactly like the Go build.
//!
//! This is the app's own seam, and the last one: the single-instance lock, the
//! debug log, the tray window and message loop, the Discord transport, the poll
//! loop and the settings window all exist in one process here. The four frozen
//! orderings are reproduced exactly:
//!
//! 1. the instance lock is taken BEFORE the logger, so a loser touches no log;
//! 2. the logger, then the client-id SOURCE only -- never the value;
//! 3. `--selftest` (via [`potflix::selftest`]) bypasses [`run_ui`] wholly;
//! 4. the tray window is created and pumped on one OS thread.
//!
//! # Where `go\events.go` lives
//!
//! The frozen module map (`src\lib.rs`) gives the app library exactly one module,
//! `selftest`. The event-line emitters from `go\events.go` are therefore exposed
//! from [`potflix::selftest`] (`event_connected`, `event_push_attempt`,
//! `event_decision`, ...) and called from here as `selftest::event_*`. That keeps
//! one writer per log line instead of duplicating the formats across the bin.
//!
//! # Known deviations (recorded, not silent)
//!
//! * `tray::clear_activity` now has an installable seam
//!   ([`tray::set_clear_activity`]); [`run_ui`] installs the dc-bound clear
//!   exactly as Go does at `main.go:197-200`, so the tray's "Exit Potflix" path
//!   sends the IPC clear instead of only logging.
//! * `window::set_app_log` and `tray::set_logger` both take a `&'static`
//!   [`Logger`]; [`run_ui`] installs the same logger in both seams, so the
//!   tray and the UI write to one shared log with no second file handle
//!   (Go's `appLog = log` at `main.go:121`).

#![windows_subsystem = "windows"]

use std::sync::{Arc, LazyLock, Mutex, OnceLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use app_support::clientid;
use app_support::config;
use app_support::debuglog::{self, Logger};
use app_support::instance;
use potflix::selftest;
use potflix_win::{tray, window};
use presence::Tracker;
use presence::gate::Gate;

/// Per-poll sleep while tracking (`go\main.go:31`).
const POLL_INTERVAL: Duration = Duration::from_millis(250);
/// Play status Undefined -> re-enumerate (`go\main.go:32`).
const UNDEFINED_SLEEP: Duration = Duration::from_millis(160);
/// No player window -> idle (`go\main.go:33`).
const NO_PLAYER_SLEEP: Duration = Duration::from_millis(1000);

/// The single metadata resolver for the poll loop, set once in [`run_ui`] before
/// the loop starts (unset in tests, which keep the clean card) (`go\main.go:42`).
static META_RESOLVER: OnceLock<metadata::Resolver> = OnceLock::new();

/// The folder-aware query cache keyed by the current raw title
/// (`go\main.go:81-85`): the poll loop is the only writer.
static META_CACHE: LazyLock<Mutex<Option<(String, metadata::Query)>>> =
    LazyLock::new(|| Mutex::new(None));

/// Wall-clock milliseconds, Go's `time.Now().UnixMilli()`.
fn now_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|since| since.as_millis() as i64)
        .unwrap_or(0)
}

/// The stored per-kind card specs: whatever the Card view last published to the
/// process-wide mirror, or the shipped defaults when nothing was published
/// yet. The poll loop reads this every poll, so a preset save takes effect on
/// the next push with no restart.
fn live_card_specs() -> app_support::config::CardSpecs {
    config::live_card_specs()
}

/// The folder-aware query for `raw_title`, querying the playing-file path once
/// per title change and caching by raw title (`go\main.go:89-102`).
fn meta_query_for(raw_title: &str) -> metadata::Query {
    if let Some((cached_title, cached_query)) = META_CACHE
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .as_ref()
        && cached_title == raw_title
    {
        return cached_query.clone();
    }
    let mut path = String::new();
    if let Some(hwnd) = player::window::find_first()
        && let Ok(p) = player::pathquery::current_file(hwnd)
    {
        path = p;
    }
    let query = metadata_core::context::resolve_query(raw_title, &path);
    *META_CACHE.lock().unwrap_or_else(|e| e.into_inner()) =
        Some((raw_title.to_string(), query.clone()));
    query
}

/// `pushActivity` (`go\main.go:332-394`): build the card, ask the gate whether it
/// must go out, log the attempt (the observation boundary), and send only when
/// the gate says so. A failed send is not fatal -- the supervisor reconnects and
/// a later decision resends -- but it is never silent.
///
/// `specs` is the stored per-kind Card view choice (`liveCardSpecs`) and
/// `record` is the preview sink (`recordPreview`), injected so this one
/// implementation is both the production path the poll loop runs and the path
/// the cardview tests drive.
///
/// The preview is recorded only AFTER a successful send (`main.go:391-392`).
#[allow(clippy::too_many_arguments)]
fn push_activity(
    log: &Logger,
    dc: &discord::Client,
    gate: &mut Gate,
    specs: app_support::config::CardSpecs,
    record: &mut dyn FnMut(
        app_support::config::CardKind,
        String,
        Option<presence::Action>,
        presence::Resolved,
    ),
    raw_title: &str,
    action: Option<&presence::Action>,
    ct: i64,
    total: i64,
    now: i64,
    event: bool,
    debounced: bool,
) {
    let query = meta_query_for(raw_title);
    let mut res = presence::Resolved::default();
    let mut found = false;
    if let Some(resolver) = META_RESOLVER.get() {
        // Non-blocking: a miss fires at most one background fetch per key.
        // Unknown kinds (themes, extras, bare names) never fetch or cache.
        if query.kind != metadata::Kind::UNKNOWN {
            let cancel = metadata_http::Cancel::background();
            if let Ok(answer) = resolver.resolve(&cancel, &query)
                && answer.found
            {
                res = presence::Resolved {
                    series: answer.series,
                    episode_name: answer.episode,
                    artwork: answer.artwork,
                    synopsis: answer.synopsis,
                    genres: answer.genres,
                    length: answer.length,
                    year: answer.year,
                    is_movie: answer.is_movie,
                    buttons: Vec::new(),
                };
                found = true;
            }
        }
    }
    // The kind follows the answer when one was found, else the query; an
    // UNKNOWN title takes the series ladder.
    let is_movie = if found {
        res.is_movie
    } else {
        query.kind == metadata::Kind::MOVIE
    };
    let kind = if is_movie {
        app_support::config::CardKind::Movie
    } else {
        app_support::config::CardKind::Series
    };
    // Always compose: with the default spec for a kind, `compose_card`
    // reproduces the builder's output byte-for-byte, so no skip branch is
    // needed.
    let card = presence::compose_card(raw_title, action, &res, is_movie, &specs.for_kind(kind));
    let (out, verdict) = gate.decide(card, ct, total, now, event, debounced);
    selftest::event_push_attempt(log, &out, &verdict);
    if !verdict.send {
        return;
    }
    // A pause parks the paused card and keeps it; the gate never sets Clear now,
    // so this branch is dormant and retained for the null-activity path only.
    if verdict.clear {
        if let Err(err) = dc.clear_activity() {
            selftest::event_send_error(log, &err);
        }
        return;
    }
    selftest::event_push(log, &out.details, &out.state, ct, total);
    let wire = discord::Activity {
        name: out.name.clone(),
        details: out.details.clone(),
        state: out.state.clone(),
        large_image: out.large_image.clone(),
        large_text: out.large_text.clone(),
        small_image: String::new(),
        small_text: String::new(),
        has_start: out.has_start,
        start: out.start,
        end: out.end,
        buttons: Vec::new(),
    };
    match dc.set_activity(wire) {
        Ok(()) => record(
            kind,
            raw_title.to_string(),
            action.cloned(),
            res.clone(),
        ),
        Err(err) => selftest::event_send_error(log, &err),
    }
}

/// `pollLoop` (`go\main.go:241-325`): re-enumerate the first PotPlayer window,
/// skip an Undefined play status for 160 ms, then drive the presence tracker
/// every 250 ms and push every decision to Discord.
fn poll_loop(log: &Logger, dc: &discord::Client) -> ! {
    // Java starts with cleared=true (Main.java:95): the first no-player pass
    // sends no clear.
    let mut cleared = true;
    let mut last_gen = -1i64;

    loop {
        // A reconnect starts a fresh Discord session. The segment below is reset
        // on every outer iteration anyway (Main.java:109), so only the generation
        // needs noting here.
        if dc.generation() != last_gen {
            last_gen = dc.generation();
        }

        let Some(hwnd) = player::window::find_first() else {
            if !cleared {
                if let Err(err) = dc.clear_activity() {
                    selftest::event_send_error(log, &err);
                }
                selftest::event_cleared_no_player(log);
                cleared = true;
            }
            std::thread::sleep(NO_PLAYER_SLEEP);
            continue;
        };

        if player::control::get_play_status(hwnd) == player::control::PlayStatus::UNDEFINED {
            std::thread::sleep(UNDEFINED_SLEEP);
            continue;
        }
        cleared = false;

        // Java makes one PresenceTracker per outer iteration (Main.java:109), so
        // a window that disappears and returns starts a fresh segment rather than
        // inheriting the previous segment's last-seen state.
        let mut tracker = Tracker::new();
        let mut gate = Gate::new();

        // Track this window until it dies or stops looking like PotPlayer.
        while player::window::alive(hwnd) {
            if dc.generation() != last_gen {
                last_gen = dc.generation();
                tracker = Tracker::new();
                gate = Gate::new();
            }
            let title = player::window::title(hwnd);
            if !player::window::matches(&title) {
                break;
            }
            let status = player::control::get_play_status(hwnd);
            let ct = player::control::get_current_time(hwnd);
            let total = player::control::get_total_time(hwnd);
            let now = now_ms();
            let action = tracker.poll(&title, status, ct, now);
            if let Some(a) = action.as_ref() {
                selftest::event_decision(
                    log,
                    now,
                    &status.to_string(),
                    ct,
                    &title,
                    a.tag(),
                    a.running,
                    a.paused,
                    a.has_start,
                    a.start,
                    &a.suffix,
                );
            }
            // The gate is consulted EVERY poll with the current card so it can
            // re-anchor on divergence, fire the heartbeat, and log the verdict.
            let debounced = tracker.pending_debounce();
            if let Some(cur) = action.clone().or_else(|| tracker.current()) {
                let mut record =
                    |kind, raw: String, act: Option<presence::Action>, res: presence::Resolved| {
                        window::ui().record_preview(kind, raw, act, res);
                    };
                push_activity(
                    log,
                    dc,
                    &mut gate,
                    live_card_specs(),
                    &mut record,
                    &title,
                    Some(&cur),
                    ct,
                    total,
                    now,
                    action.is_some(),
                    debounced,
                );
            }
            std::thread::sleep(POLL_INTERVAL);
        }

        if let Err(err) = dc.clear_activity() {
            selftest::event_send_error(log, &err);
        }
        selftest::event_cleared_window_gone(log);
    }
}

/// `runUI` (`go\main.go:137-216`): build the hidden window + tray icon, start the
/// Discord transport and the poll loop, then pump messages until exit.
///
/// The tray window and the message loop must share one OS thread; this runs on
/// the process main thread, so that thread law holds without `LockOSThread`.
fn run_ui(log: &'static Logger) {
    let tray_window = match tray::create_tray_window() {
        Ok(window) => window,
        Err(err) => {
            log.log(&format!("tray init failed: {err}"));
            return;
        }
    };
    let tray_failed = !tray_window.added;
    if tray_failed {
        // Deliberate departure from Java :550-551 (see README deviation 16):
        // a tray-less process keeps serving the card with the window visible
        // instead of exiting silently.
        eprintln!("TrayIcon could not be added.");
        log.log("notify-icon add result=false");
    } else {
        log.log(&format!(
            "notify-icon add result=true tooltip=Potflix hwnd=0x{:x} icon=0x{:x}",
            tray_window.hwnd as usize, tray_window.icon as usize
        ));
    }

    // Tray is up: start the Discord transport and the poll loop on their own
    // threads; the message loop below owns this OS thread.
    let (client_id, _) = clientid::resolve_client_id();
    let dc = discord::Client::new(discord::Config {
        client_id: client_id.to_string(),
        on_connect: Some(Box::new(move || selftest::event_connected(log))),
        on_reply: Some(Box::new(move |reply: discord::Reply| {
            if reply.error {
                selftest::event_push_error(
                    log,
                    &reply.nonce,
                    &reply.evt,
                    reply.code,
                    &reply.message,
                );
            } else {
                selftest::event_push_ack(log, &reply.nonce);
            }
        })),
        on_ack_timeout: Some(Box::new(move |nonce: &str| {
            selftest::event_push_ack_timeout(log, nonce);
        })),
        on_drop: Some(Box::new(move |err: &discord::Error| {
            selftest::event_session_dropped(log, err);
        })),
        ..Default::default()
    });
    dc.start();

    let cfg = config::load_config();
    config::set_live_card_specs(cfg.effective_card_specs());
    metadata_http::set_rate_profile(cfg.effective_scan_speed());
    log.log(&format!(
        "scan speed profile: {}",
        cfg.effective_scan_speed()
    ));

    let logf: metadata::resolver::LogFn = Arc::new(move |message: &str| log.log(message));
    let _ = META_RESOLVER.set(metadata::Resolver::new(metadata::Options {
        logf: Some(logf),
        ..Default::default()
    }));

    // Library catch-up: resolve whatever metadata the store-backed index still
    // owes, on a background worker, resuming where the last run stopped.
    let _ = library::catchup::start_catch_up(
        library::catchup::CatchUpConfig {
            dirs: cfg.dirs.clone(),
            cache_dir: config::metadata_dir().to_string_lossy().into_owned(),
            index_path: config::scan_index_path().to_string_lossy().into_owned(),
            ..Default::default()
        },
        None,
    );

    let dc = Arc::new(dc);
    let poll_dc = Arc::clone(&dc);
    std::thread::spawn(move || poll_loop(log, &poll_dc));

    // Fleet decision 6 (deviation): clear the Discord card before exiting. Go
    // installs the dc-bound override at `go\main.go:197-200`, replacing the tray
    // stub; the poll loop's own clears are already live, so this repairs only
    // the tray Exit path.
    let clear_dc = Arc::clone(&dc);
    let _ = tray::set_clear_activity(Box::new(move || {
        let _ = clear_dc.clear_activity();
        log.log("cleared (exit)");
    }));

    // Settings window: created hidden, shown on manual launch, kept hidden to
    // the tray on the autostart path. `--selftest` never reaches here.
    match window::create_main_window() {
        Err(err) => log.log(&format!("main window init failed: {err}")),
        Ok(()) => {
            let args: Vec<String> = std::env::args().collect();
            if tray_failed {
                // No tray icon, no tray menu, no other way back in: leave the
                // window visible so the app is reachable and quittable (X).
                log.log("tray icon missing: window left visible");
                window::show_main_window();
            } else if potflix_win::settings::should_show_main_window(&args) {
                window::show_main_window();
            } else {
                log.log("autostart launch: window kept hidden (tray only)");
            }
        }
    }

    tray::message_loop();
    let _ = dc.close();
}

fn main() {
    // argv is ignored except for the M1 test switch.
    let args: Vec<String> = std::env::args().collect();
    let selftest_run = potflix_win::settings::has_arg(&args, "--selftest");
    if selftest_run {
        selftest::attach_console(); // make --selftest output visible from the parent console
    }

    // (1) Single-instance lock: 127.0.0.1:17843, held for life. Taken before
    // anything else (Java :61), so a second instance touches no log.
    if instance::acquire_instance_lock().is_err() {
        instance::exit_already_running();
    }

    // (2) Debug log beside the executable.
    let log: &'static Logger = Box::leak(Box::new(Logger::new(
        debuglog::default_log_path(),
        debuglog::DEFAULT_LOG_CAP_BYTES,
        debuglog::DEFAULT_LOG_MAX_FILES,
    )));
    log.log(&format!(
        "instance lock acquired {}:{} backlog={}",
        instance::LOCK_HOST,
        instance::LOCK_PORT,
        instance::LOCK_BACKLOG
    ));

    // (3) Client id: log only the source, never the value.
    let (_, source) = clientid::resolve_client_id();
    clientid::log_client_id_source(log, source);

    // `appLog = log` for the tray window's message/menu callbacks and the
    // settings window's `app_logf` (Go `main.go:121`: one shared logger).
    let _ = tray::set_logger(log);
    let _ = window::set_app_log(log);
    let _ = tray::set_show_main_window(window::show_main_window);

    if selftest_run {
        selftest::run_self_test(log);
    }

    run_ui(log);
    log.close();
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::mpsc;
    use std::time::Instant;

    /// Serializes the tests that publish to or read the process-wide live card
    /// mirror: a concurrent publisher would leak into another test's read, and
    /// a concurrent reader would observe mid-flight specs.
    static LIVE_SERIAL: Mutex<()> = Mutex::new(());

    use player::control::PlayStatus;

    fn unique_dir(tag: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "potflix-s13-{tag}-{}-{}",
            std::process::id(),
            now_ms()
        ));
        let _ = std::fs::create_dir_all(&dir);
        dir
    }

    /// `gate_integration_test.go` -> `TestGateFakePipeSequence`: headless
    /// end-to-end proof of the push gate through the in-process fake pipe. It
    /// drives the REAL tracker, gate and pushActivity over a scripted position
    /// sequence and asserts the exact SET_ACTIVITY frames.
    #[test]
    fn gate_fake_pipe_sequence() {
        // Reads the live mirror on every drive; the serial keeps a concurrent
        // publisher out of this long run.
        let _live_serial = LIVE_SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        const IT_TITLE: &str = "Breaking Bad (2008) - S04E07 - Problem Dog (1080p BluRay x265 Silence).mkv - PotPlayer";
        const IT_TOTAL: i64 = 2_832_979;
        const IT_N: i64 = 1_000_000_000_000;

        let srv = discord::FakeServer::new(0);
        let base = srv.base.clone();
        let (accept_tx, accept_rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = accept_tx.send(srv.accept().map_err(|e| e.to_string()));
        });

        let dc = discord::Client::new(discord::Config {
            client_id: "123456789012345678".to_string(),
            pipe_base: base,
            retry_delay: Duration::from_millis(30),
            ..Default::default()
        });
        dc.start();

        let sc = match accept_rx.recv_timeout(Duration::from_secs(5)) {
            Ok(Ok(sc)) => sc,
            Ok(Err(err)) => panic!("fake server accept: {err}"),
            Err(_) => panic!("client did not connect to the fake pipe"),
        };

        let (op, _) = sc.read_frame().expect("handshake frame");
        assert_eq!(op, discord::OP_HANDSHAKE, "first frame is the handshake");
        sc.write_frame(
            discord::OP_FRAME,
            br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#,
        )
        .expect("write READY");
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        while dc.generation() < 1 {
            assert!(
                std::time::Instant::now() < deadline,
                "client never became ready after the READY dispatch"
            );
            std::thread::sleep(Duration::from_millis(2));
        }

        // Collect every post-READY frame the client sends.
        let (frames_tx, frames_rx) = mpsc::channel::<Vec<u8>>();
        let reader = std::thread::spawn(move || {
            loop {
                match sc.read_frame() {
                    Ok((discord::OP_FRAME, payload)) => {
                        if frames_tx.send(payload).is_err() {
                            return;
                        }
                    }
                    Ok(_) => {}
                    Err(_) => return,
                }
            }
        });

        let dir = unique_dir("gate");
        let log_path = dir.join("gate-integration.log");
        let log = Logger::new(&log_path, 0, 1);

        let mut tracker = Tracker::new();
        let mut gate = Gate::new();
        {
            let mut drive = |status: PlayStatus, ct: i64, now: i64| {
                let action = tracker.poll(IT_TITLE, status, ct, now);
                let debounced = tracker.pending_debounce();
                if let Some(cur) = action.clone().or_else(|| tracker.current()) {
                    let mut record =
                        |kind, raw: String, act: Option<presence::Action>, res: presence::Resolved| {
                            window::ui().record_preview(kind, raw, act, res);
                        };
                    push_activity(
                        &log,
                        &dc,
                        &mut gate,
                        live_card_specs(),
                        &mut record,
                        IT_TITLE,
                        Some(&cur),
                        ct,
                        IT_TOTAL,
                        now,
                        action.is_some(),
                        debounced,
                    );
                }
            };

            const R: PlayStatus = PlayStatus::RUNNING;
            const P: PlayStatus = PlayStatus::PAUSED;

            drive(R, 1000, IT_N); // 0 first card -> sent
            drive(R, 1250, IT_N + 250); // 1 steady -> suppressed heartbeat-not-due
            drive(R, 2800, IT_N + 500); // 2 small seek inside the window -> held
            drive(R, 2800, IT_N + 750); // 3 still held -> suppressed debounce
            drive(R, 2800, IT_N + 1000); // 4 trailing edge lands -> sent
            drive(R, 3050, IT_N + 1250); // 5 steady
            drive(R, 9000, IT_N + 1500); // 6 big seek inside window -> divergence sent
            drive(R, 9250, IT_N + 1750); // 7 steady
            drive(R, 15000, IT_N + 2000); // 8 seek after window -> tracker event sent
            drive(R, 15250, IT_N + 2250); // 9 steady
            drive(P, 15250, IT_N + 2500); // 10 flap -> still running
            drive(P, 15250, IT_N + 2750); // 11 real pause -> parked paused card
            drive(P, 15250, IT_N + 3000); // 12 paused steady
            drive(R, 15250, IT_N + 3250); // 13 resume -> sent
            for i in 1..=120i64 {
                drive(R, 15250 + 250 * i, IT_N + 3250 + 250 * i);
            }
        }

        // Positive control: force one push through the real path.
        gate.force_next();
        let cur = tracker.current();
        {
            let mut record =
                |kind, raw: String, act: Option<presence::Action>, res: presence::Resolved| {
                    window::ui().record_preview(kind, raw, act, res);
                };
            push_activity(
                &log,
                &dc,
                &mut gate,
                live_card_specs(),
                &mut record,
                IT_TITLE,
                cur.as_ref(),
                15250 + 250 * 120,
                IT_TOTAL,
                IT_N + 3250 + 250 * 120,
                false,
                false,
            );
        }

        std::thread::sleep(Duration::from_millis(200));

        #[derive(PartialEq, Debug, Clone, Default)]
        struct Sent {
            clear: bool,
            name: String,
            details: String,
            state: String,
            has_start: bool,
            start: i64,
            has_end: bool,
            end: i64,
        }

        let mut got: Vec<Sent> = Vec::new();
        let mut raws: Vec<String> = Vec::new();
        while let Ok(payload) = frames_rx.try_recv() {
            raws.push(String::from_utf8_lossy(&payload).into_owned());
            let wire: serde_json::Value = serde_json::from_slice(&payload).expect("decode frame");
            assert_eq!(wire["cmd"], "SET_ACTIVITY", "unexpected frame");
            if wire["args"]["activity"].is_null() {
                got.push(Sent {
                    clear: true,
                    ..Sent::default()
                });
                continue;
            }
            let activity = &wire["args"]["activity"];
            let mut sent = Sent {
                name: activity["name"].as_str().unwrap_or("").to_string(),
                details: activity["details"].as_str().unwrap_or("").to_string(),
                state: activity["state"].as_str().unwrap_or("").to_string(),
                ..Sent::default()
            };
            if let Some(start) = activity["timestamps"]["start"].as_i64() {
                sent.has_start = true;
                sent.start = start;
            }
            if let Some(end) = activity["timestamps"]["end"].as_i64() {
                sent.has_end = true;
                sent.end = end;
            }
            got.push(sent);
        }

        const START0: i64 = IT_N - 1000;
        const START1: i64 = IT_N - 1800;
        const START2: i64 = IT_N - 7500;
        const START3: i64 = IT_N - 13000;
        const START4: i64 = IT_N - 12000;
        const PAUSE_START: i64 = IT_N + 2750;
        const END0: i64 = IT_N + (IT_TOTAL - 1000);
        const END1: i64 = IT_N + 1000 + (IT_TOTAL - 2800);
        const END2: i64 = IT_N + 1500 + (IT_TOTAL - 9000);
        const END3: i64 = IT_N + 2000 + (IT_TOTAL - 15000);
        const END5: i64 = IT_N + 3250 + (IT_TOTAL - 15250);
        const END6: i64 = IT_N + 33250 + (IT_TOTAL - 45250);

        let playing = |details: &str, state: &str, start: i64, end: i64| Sent {
            clear: false,
            name: "Breaking Bad".to_string(),
            details: details.to_string(),
            state: state.to_string(),
            has_start: true,
            start,
            has_end: true,
            end,
        };
        let want = [
            playing("Problem Dog", "S4 E7", START0, END0),
            playing("Problem Dog", "S4 E7", START1, END1),
            playing("Problem Dog", "S4 E7", START2, END2),
            playing("Problem Dog", "S4 E7", START3, END3),
            Sent {
                clear: false,
                name: "Breaking Bad".to_string(),
                details: "Problem Dog".to_string(),
                state: "S4 E7 - Paused".to_string(),
                has_start: true,
                start: PAUSE_START,
                has_end: false,
                end: 0,
            },
            playing("Problem Dog", "S4 E7", START4, END5),
            playing("Problem Dog", "S4 E7", START4, END6),
            playing("Problem Dog", "S4 E7", START4, END6),
        ];

        assert_eq!(
            got.len(),
            want.len(),
            "sent {} frames, want {}\n got: {got:?}",
            got.len(),
            want.len()
        );
        for (i, wanted) in want.iter().enumerate() {
            assert_eq!(&got[i], wanted, "frame {i} raw: {}", raws[i]);
        }

        // R1: playing frames carry BOTH timestamps; the paused frame carries
        // start only, never end.
        for (i, sent) in got.iter().enumerate() {
            if i == 4 {
                assert!(!sent.has_end, "paused frame {i} carries end: {}", raws[i]);
                continue;
            }
            assert!(
                sent.has_start && sent.has_end,
                "playing frame {i} must carry both timestamps: {}",
                raws[i]
            );
        }
        // R2: the pause frame is a parked paused card, and NO frame is a clear.
        assert!(
            !got[4].clear && got[4].name == "Breaking Bad" && got[4].state == "S4 E7 - Paused",
            "frame 4 is not the parked paused card: {got:?}"
        );
        for (i, sent) in got.iter().enumerate() {
            assert!(
                !sent.clear,
                "frame {i} is an activity:null clear, want a card"
            );
        }
        for (i, raw) in raws.iter().enumerate() {
            assert!(
                !raw.contains("00:15"),
                "frame {i} looks like a frozen-position paused card: {raw}"
            );
        }
        const RESUME_NOW: i64 = IT_N + 3250;
        const RESUME_CT: i64 = 15250;
        assert_eq!(
            got[5].start,
            RESUME_NOW - RESUME_CT,
            "resume start must re-anchor to now-ct"
        );
        const SEEK_NOW: i64 = IT_N + 2000;
        const SEEK_CT: i64 = 15000;
        assert_eq!(
            got[3].start,
            SEEK_NOW - SEEK_CT,
            "post-seek start must re-anchor to now-ct"
        );

        // The observation boundary: every poll logged an attempt, suppressed
        // polls included, with the reason.
        let log_text = std::fs::read_to_string(&log_path).expect("read log");
        const TOTAL_POLLS: usize = 14 + 120 + 1;
        assert_eq!(
            log_text.matches("gate=sent").count(),
            want.len(),
            "log gate=sent lines (every frame, the parked pause included)"
        );
        assert_eq!(log_text.matches("gate=clear").count(), 0, "no clear lines");
        assert_eq!(
            log_text.matches("gate=suppressed").count(),
            TOTAL_POLLS - want.len(),
            "gate=suppressed lines"
        );
        assert!(
            log_text.contains("reason=debounce"),
            "missing reason=debounce"
        );
        assert!(
            log_text.contains("reason=heartbeat-not-due"),
            "missing reason=heartbeat-not-due"
        );
        assert!(log_text.contains("reason=paused"), "missing reason=paused");

        // F1: the logged payload must agree with the frame on the wire.
        let mut logged_ends: Vec<(bool, i64)> = Vec::new();
        for line in log_text.split('\n') {
            if !line.contains("gate=sent") {
                continue;
            }
            let at = line.find("payload=").expect("sent line carries payload=");
            let decoded: serde_json::Value =
                serde_json::from_str(line[at + "payload=".len()..].trim())
                    .unwrap_or_else(|e| panic!("decode logged payload {line:?}: {e}"));
            match decoded["timestamps"]["end"].as_i64() {
                Some(end) => logged_ends.push((true, end)),
                None => logged_ends.push((false, 0)),
            }
        }
        assert_eq!(
            logged_ends.len(),
            got.len(),
            "logged sent payloads (one per frame)"
        );
        for (i, sent) in got.iter().enumerate() {
            assert!(
                logged_ends[i].0 == sent.has_end && (!sent.has_end || logged_ends[i].1 == sent.end),
                "frame {i}: logged end must match the wire"
            );
        }

        let _ = dc.close();
        let _ = reader.join();
        drop(log);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// `m3_reply_test.go` -> `TestPushReplyLoggedVerbatim`: proves the reply path
    /// end to end over the in-process fake pipe.
    #[test]
    fn push_reply_logged_verbatim() {
        fn wait_log_contains(path: &std::path::Path, want: &str, within: Duration) {
            let deadline = std::time::Instant::now() + within;
            loop {
                let text = std::fs::read_to_string(path).unwrap_or_default();
                if text.contains(want) {
                    return;
                }
                assert!(
                    std::time::Instant::now() < deadline,
                    "log {} missing {want:?}; contents:\n{text}",
                    path.display()
                );
                std::thread::sleep(Duration::from_millis(10));
            }
        }

        fn frame_nonce(payload: &[u8]) -> String {
            let value: serde_json::Value = serde_json::from_slice(payload).expect("decode frame");
            let nonce = value["nonce"].as_str().unwrap_or("").to_string();
            assert!(!nonce.is_empty(), "frame carries no nonce");
            nonce
        }

        let dir = unique_dir("m3-reply");
        let log_path = dir.join("potflix-m3-reply.log");
        // The client callbacks must be `'static`; the log is leaked so the ONE
        // handle on the file is shared by every callback (never a second handle).
        let log: &'static Logger = Box::leak(Box::new(Logger::new(&log_path, 1 << 20, 2)));

        let srv = discord::FakeServer::new(0);
        let base = srv.base.clone();
        const CLIENT_ID: &str = "123456789012345678";
        let (accept_tx, accept_rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = accept_tx.send(srv.accept().map_err(|e| e.to_string()));
        });

        let c = discord::Client::new(discord::Config {
            client_id: CLIENT_ID.to_string(),
            pipe_base: base,
            retry_delay: Duration::from_millis(30),
            on_connect: Some(Box::new(move || selftest::event_connected(log))),
            on_reply: Some(Box::new(move |reply: discord::Reply| {
                if reply.error {
                    selftest::event_push_error(
                        log,
                        &reply.nonce,
                        &reply.evt,
                        reply.code,
                        &reply.message,
                    );
                } else {
                    selftest::event_push_ack(log, &reply.nonce);
                }
            })),
            ..Default::default()
        });
        c.start();

        let sc = match accept_rx.recv_timeout(Duration::from_secs(5)) {
            Ok(Ok(sc)) => sc,
            Ok(Err(err)) => panic!("fake server accept: {err}"),
            Err(_) => panic!("client did not connect to the fake pipe"),
        };

        let (op, _) = sc.read_frame().expect("handshake frame");
        assert_eq!(op, discord::OP_HANDSHAKE);

        let act = discord::Activity {
            details: "Show".to_string(),
            state: r#"S1 E2 "Pilot""#.to_string(),
            small_image: "play".to_string(),
            has_start: true,
            start: 12345,
            ..Default::default()
        };

        // Before Discord's READY dispatch a command is refused, not written.
        match c.set_activity(act.clone()) {
            Err(discord::Error::NotReady) => {}
            other => panic!("pre-READY SetActivity = {other:?}, want NotReady"),
        }

        sc.write_frame(
            discord::OP_FRAME,
            br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#,
        )
        .expect("write READY");
        wait_log_contains(
            &log_path,
            "connected clientId=<redacted>",
            Duration::from_secs(3),
        );

        // (1) a clean reply logs `push ack nonce=<n>`.
        c.set_activity(act.clone()).expect("SetActivity");
        let (op, frame) = sc.read_frame().expect("read SET_ACTIVITY");
        assert_eq!(op, discord::OP_FRAME);
        let nonce1 = frame_nonce(&frame);
        let ack = format!(r#"{{"cmd":"SET_ACTIVITY","evt":null,"nonce":"{nonce1}","data":null}}"#);
        sc.write_frame(discord::OP_FRAME, ack.as_bytes())
            .expect("write ack");
        wait_log_contains(
            &log_path,
            &format!("push ack nonce={nonce1}"),
            Duration::from_secs(3),
        );

        // (2) Discord's ERROR frame is logged verbatim, client id redacted.
        c.set_activity(act).expect("SetActivity 2");
        let (op, frame) = sc.read_frame().expect("read SET_ACTIVITY 2");
        assert_eq!(op, discord::OP_FRAME);
        let nonce2 = frame_nonce(&frame);
        let raw = format!("Invalid activity for client {CLIENT_ID}: bad assets");
        let err_frame = format!(
            r#"{{"cmd":"SET_ACTIVITY","evt":"ERROR","nonce":"{nonce2}","data":{{"code":4003,"message":"{raw}"}}}}"#
        );
        sc.write_frame(discord::OP_FRAME, err_frame.as_bytes())
            .expect("write error frame");
        let want = format!(
            "push error nonce={nonce2} evt=ERROR code=4003 message=Invalid activity for client <redacted>: bad assets"
        );
        wait_log_contains(&log_path, &want, Duration::from_secs(3));

        let _ = c.close();
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The cardview test, repointed to the APP's `pushActivity`
    /// (`go\main.go:332-394`): drives the ONE production implementation over a
    /// fake pipe and asserts the preview composes to the payload the send path
    /// produced, under the stored per-kind Card view specs (`specs` param =
    /// `liveCardSpecs`).
    #[test]
    fn preview_is_payload_through_send() {
        let srv = discord::FakeServer::new(0);
        let base = srv.base.clone();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(srv.accept());
        });
        let dc = discord::Client::new(discord::Config {
            client_id: "123456789012345678".into(),
            pipe_base: base,
            retry_delay: Duration::from_millis(30),
            ..Default::default()
        });
        dc.start();
        let sc = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("client connects")
            .expect("accept");
        let (op, _) = sc.read_frame().expect("handshake frame");
        assert_eq!(op, discord::OP_HANDSHAKE);
        sc.write_frame(
            discord::OP_FRAME,
            br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#,
        )
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        while dc.generation() < 1 {
            assert!(Instant::now() < deadline, "never became ready");
            std::thread::sleep(Duration::from_millis(2));
        }
        let (ftx, frx) = mpsc::channel::<Vec<u8>>();
        let reader = std::thread::spawn(move || {
            loop {
                match sc.read_frame() {
                    Ok((discord::OP_FRAME, payload)) => {
                        if ftx.send(payload).is_err() {
                            return;
                        }
                    }
                    Ok(_) => {}
                    Err(_) => return,
                }
            }
        });

        let dir = unique_dir("preview");
        let log = Logger::new(dir.join("preview.log"), 0, 1);
        let now = 1_000_000_000_000i64;
        let drive = |specs: config::CardSpecs,
                     raw: &str,
                     act: &presence::Action,
                     ct: i64|
         -> (presence::Activity, serde_json::Value, Vec<u8>) {
            let mut st = window::UiState::default();
            st.card_specs_state = specs.clone();
            let mut gate = Gate::new();
            let mut record =
                |kind, raw: String, a: Option<presence::Action>, res: presence::Resolved| {
                    st.record_preview(kind, raw, a, res);
                };
            push_activity(
                &log,
                &dc,
                &mut gate,
                specs,
                &mut record,
                raw,
                Some(act),
                ct,
                2_832_979,
                now,
                true,
                false,
            );
            drop(record);
            let payload = frx
                .recv_timeout(Duration::from_secs(2))
                .expect("SET_ACTIVITY frame");
            let v: serde_json::Value = serde_json::from_slice(&payload).unwrap();
            assert_eq!(v["cmd"], "SET_ACTIVITY");
            assert!(!v["args"]["activity"].is_null());
            // The push records exactly one kind; the preview for that kind must
            // compose to the payload. Probe both kinds: exactly one is present.
            let mut matched: Option<presence::Activity> = None;
            for kind in [config::CardKind::Series, config::CardKind::Movie] {
                st.set_card_kind(kind);
                let (got, have) = st.preview_activity();
                if have {
                    assert!(
                        matched.is_none(),
                        "both kinds recorded for {raw:?}, want exactly one"
                    );
                    matched = Some(got);
                }
            }
            let got = matched.expect("preview not recorded after a successful send");
            (got, v, payload)
        };
        let check = |got: &presence::Activity, v: &serde_json::Value| {
            let a = &v["args"]["activity"];
            assert_eq!(got.name, a["name"].as_str().unwrap_or(""));
            assert_eq!(got.details, a["details"].as_str().unwrap_or(""));
            assert_eq!(got.state, a["state"].as_str().unwrap_or(""));
            let assets = &a["assets"];
            let mut want_large = got.large_image.clone();
            if want_large != "logo" && !want_large.starts_with("http") {
                want_large = "logo".into();
            }
            if want_large.is_empty() {
                want_large = "logo".into();
            }
            assert_eq!(assets["large_image"].as_str().unwrap_or(""), want_large);
            assert_eq!(assets["large_text"].as_str().unwrap_or(""), got.large_text);
            // The preview composes through the same composer (name, lines,
            // assets, start anchor) but carries no gate projection: `end` is
            // computed by the send gate from ct/total, not from the recorded
            // inputs, so the preview's `end` stays 0 while a running wire
            // frame carries the projection.
            if got.has_start {
                assert_eq!(a["timestamps"]["start"].as_i64(), Some(got.start));
            }
            assert_eq!(got.end, 0, "preview carries no gate-projected end");
            assert!(
                a["timestamps"]["end"].as_i64().is_some(),
                "running wire frame must carry the gate-projected end"
            );
            assert_eq!(
                a["buttons"].as_array().map_or(0, Vec::len),
                got.buttons.len()
            );
        };

        let series_raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
        let act = presence::Action {
            running: true,
            has_start: true,
            start: now - 1000,
            ct: 1000,
            ..presence::Action::default()
        };
        let (got, v, _) = drive(config::CardSpecs::default(), series_raw, &act, 1000);
        check(&got, &v);

        let act2 = presence::Action {
            running: true,
            has_start: true,
            start: now - 2000,
            ct: 2000,
            ..presence::Action::default()
        };
        let mut series_specs = config::CardSpecs::default();
        assert!(series_specs.set_line(
            config::CardKind::Series,
            config::CardLine::Details,
            "title"
        ));
        assert!(series_specs.set_line(config::CardKind::Series, config::CardLine::State, "tag"));
        let (got2, v2, _) = drive(series_specs, series_raw, &act2, 2000);
        check(&got2, &v2);
        assert!(!got2.details.is_empty() && !got2.state.is_empty());

        let act3 = presence::Action {
            running: true,
            has_start: true,
            start: now - 3000,
            ct: 3000,
            ..presence::Action::default()
        };
        let mut minimal_specs = config::CardSpecs::default();
        assert!(minimal_specs.set_line(
            config::CardKind::Series,
            config::CardLine::State,
            "none"
        ));
        let (got3, v3, frame) = drive(minimal_specs, series_raw, &act3, 3000);
        check(&got3, &v3);
        assert_eq!(got3.state, "");
        let env: serde_json::Value = serde_json::from_slice(&frame).unwrap();
        assert!(
            env["args"]["activity"].get("state").is_none(),
            "empty state reached the wire"
        );
        assert_eq!(env["args"]["activity"]["details"], "Pilot");

        let _ = dc.close();
        let _ = reader.join();
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The per-kind wire test: a movie title composes with the movie spec and a
    /// series title with the series spec, through the ONE production push path
    /// with no resolver (kind from the query).
    #[test]
    fn per_kind_specs_reach_the_wire() {
        let srv = discord::FakeServer::new(0);
        let base = srv.base.clone();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(srv.accept());
        });
        let dc = discord::Client::new(discord::Config {
            client_id: "123456789012345678".into(),
            pipe_base: base,
            retry_delay: Duration::from_millis(30),
            ..Default::default()
        });
        dc.start();
        let sc = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("client connects")
            .expect("accept");
        let (op, _) = sc.read_frame().expect("handshake frame");
        assert_eq!(op, discord::OP_HANDSHAKE);
        sc.write_frame(
            discord::OP_FRAME,
            br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#,
        )
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        while dc.generation() < 1 {
            assert!(Instant::now() < deadline, "never became ready");
            std::thread::sleep(Duration::from_millis(2));
        }
        let (ftx, frx) = mpsc::channel::<Vec<u8>>();
        let reader = std::thread::spawn(move || {
            loop {
                match sc.read_frame() {
                    Ok((discord::OP_FRAME, payload)) => {
                        if ftx.send(payload).is_err() {
                            return;
                        }
                    }
                    Ok(_) => {}
                    Err(_) => return,
                }
            }
        });

        let dir = unique_dir("per-kind");
        let log = Logger::new(dir.join("per-kind.log"), 0, 1);
        let now = 1_000_000_000_000i64;
        // Series keeps a visible state; movies go minimal. With no resolver the
        // inputs are empty, so `none` (empty) is the only state preset that
        // stays distinct on both ladders.
        let mut specs = config::CardSpecs::default();
        assert!(specs.set_line(config::CardKind::Series, config::CardLine::State, "tag"));
        assert!(specs.set_line(config::CardKind::Movie, config::CardLine::State, "none"));

        let drive_raw = |raw: &str, ct: i64| -> serde_json::Value {
            let mut st = window::UiState::default();
            st.card_specs_state = specs.clone();
            let mut gate = Gate::new();
            let act = presence::Action {
                running: true,
                has_start: true,
                start: now - ct,
                ct,
                ..presence::Action::default()
            };
            let mut record =
                |kind, raw: String, a: Option<presence::Action>, res: presence::Resolved| {
                    st.record_preview(kind, raw, a, res);
                };
            push_activity(
                &log,
                &dc,
                &mut gate,
                specs.clone(),
                &mut record,
                raw,
                Some(&act),
                ct,
                2_832_979,
                now,
                true,
                false,
            );
            drop(record);
            let payload = frx
                .recv_timeout(Duration::from_secs(2))
                .expect("SET_ACTIVITY frame");
            let v: serde_json::Value = serde_json::from_slice(&payload).unwrap();
            assert_eq!(v["cmd"], "SET_ACTIVITY");
            // The preview for the recorded kind equals the wire payload.
            let mut matched = false;
            for kind in [config::CardKind::Series, config::CardKind::Movie] {
                st.set_card_kind(kind);
                let (got, have) = st.preview_activity();
                if have {
                    assert!(!matched, "both kinds recorded for {raw:?}");
                    assert_eq!(
                        got.details,
                        v["args"]["activity"]["details"].as_str().unwrap_or("")
                    );
                    if got.state.is_empty() {
                        assert!(
                            v["args"]["activity"].get("state").is_none(),
                            "empty state reached the wire for {raw:?}"
                        );
                    } else {
                        assert_eq!(
                            got.state,
                            v["args"]["activity"]["state"].as_str().unwrap_or("")
                        );
                    }
                    matched = true;
                }
            }
            assert!(matched, "no preview recorded for {raw:?}");
            v
        };

        let series_raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
        let movie_raw = "Dune.Part.Two.2024.1080p.BluRay.x264.mkv - PotPlayer";
        // Positive control: the query kinds really are series vs movie here.
        assert_eq!(
            meta_query_for(series_raw).kind,
            metadata::Kind::SERIES,
            "series fixture query kind"
        );
        assert_eq!(
            meta_query_for(movie_raw).kind,
            metadata::Kind::MOVIE,
            "movie fixture query kind"
        );

        let v_series = drive_raw(series_raw, 1000);
        assert!(
            v_series["args"]["activity"].get("state").is_some(),
            "series title must carry the series (tag) state, got {v_series}"
        );
        let v_movie = drive_raw(movie_raw, 2000);
        assert!(
            v_movie["args"]["activity"].get("state").is_none(),
            "movie title must carry the movie (none) state, got {v_movie}"
        );

        let _ = dc.close();
        let _ = reader.join();
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// `window.rs` -> the failure-side cardview test, repointed to the APP's
    /// `pushActivity`: a failed send records no preview.
    #[test]
    fn preview_not_recorded_on_send_failure() {
        let dir = unique_dir("preview-fail");
        let dc = discord::Client::new(discord::Config {
            client_id: "123456789012345678".into(),
            pipe_base: format!("{}-no-pipe-", dir.display()),
            retry_delay: Duration::from_secs(3600),
            ..Default::default()
        });
        let log = Logger::new(dir.join("preview-fail.log"), 0, 1);
        let mut st = window::UiState::default();
        let mut gate = Gate::new();
        let act = presence::Action {
            running: true,
            has_start: true,
            start: 999,
            ct: 1000,
            ..presence::Action::default()
        };
        let mut record =
            |kind, raw: String, a: Option<presence::Action>, res: presence::Resolved| {
                st.record_preview(kind, raw, a, res);
            };
        push_activity(
            &log,
            &dc,
            &mut gate,
            config::CardSpecs::default(),
            &mut record,
            "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
            Some(&act),
            1000,
            2_832_979,
            1_000_000_000_000,
            true,
            false,
        );
        drop(record);
        assert!(
            !st.preview_activity().1,
            "preview recorded after a failed send"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The real push path over an in-process fake pipe: the caller drives
    /// `push_activity` and reads the exact SET_ACTIVITY frames. Returns the
    /// client, the frame channel, the reader thread, and the scratch dir the
    /// log lives in; the caller closes, joins, and removes them.
    fn live_pipe_harness(
        tag: &str,
    ) -> (
        discord::Client,
        mpsc::Receiver<Vec<u8>>,
        std::thread::JoinHandle<()>,
        std::path::PathBuf,
    ) {
        let srv = discord::FakeServer::new(0);
        let base = srv.base.clone();
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let _ = tx.send(srv.accept());
        });
        let dc = discord::Client::new(discord::Config {
            client_id: "123456789012345678".into(),
            pipe_base: base,
            retry_delay: Duration::from_millis(30),
            ..Default::default()
        });
        dc.start();
        let sc = rx
            .recv_timeout(Duration::from_secs(5))
            .expect("client connects")
            .expect("accept");
        let (op, _) = sc.read_frame().expect("handshake frame");
        assert_eq!(op, discord::OP_HANDSHAKE);
        sc.write_frame(
            discord::OP_FRAME,
            br#"{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}"#,
        )
        .unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        while dc.generation() < 1 {
            assert!(Instant::now() < deadline, "never became ready");
            std::thread::sleep(Duration::from_millis(2));
        }
        let (ftx, frx) = mpsc::channel::<Vec<u8>>();
        let reader = std::thread::spawn(move || {
            loop {
                match sc.read_frame() {
                    Ok((discord::OP_FRAME, payload)) => {
                        if ftx.send(payload).is_err() {
                            return;
                        }
                    }
                    Ok(_) => {}
                    Err(_) => return,
                }
            }
        });
        let dir = unique_dir(tag);
        (dc, frx, reader, dir)
    }

    /// The body lines of one SET_ACTIVITY frame: details plus the state when
    /// the wire carries it (an empty state is omitted, never sent blank).
    fn wire_lines(payload: &[u8]) -> (String, Option<String>) {
        let v: serde_json::Value = serde_json::from_slice(payload).expect("decode frame");
        assert_eq!(v["cmd"], "SET_ACTIVITY");
        let a = &v["args"]["activity"];
        (
            a["details"].as_str().unwrap_or("").to_string(),
            a.get("state")
                .and_then(|s| s.as_str())
                .map(str::to_string),
        )
    }

    /// What `push_activity` must compose for these inputs under these specs (no
    /// resolver in tests, so the resolved inputs are empty and the kind follows
    /// the query).
    fn expect_lines(
        raw: &str,
        act: &presence::Action,
        specs: &config::CardSpecs,
    ) -> (String, String) {
        let is_movie = meta_query_for(raw).kind == metadata::Kind::MOVIE;
        let kind = if is_movie {
            config::CardKind::Movie
        } else {
            config::CardKind::Series
        };
        let card = presence::compose_card(
            raw,
            Some(act),
            &presence::Resolved::default(),
            is_movie,
            &specs.for_kind(kind),
        );
        (card.details, card.state)
    }

    /// The Card-view-preset regression test on the real path: push once under
    /// the published specs, publish a different spec as a save would, and the
    /// NEXT poll with identical inputs sends the new lines -- no restart, no
    /// force flag, no re-push. Series and movie variants (the specs are per
    /// kind).
    #[test]
    fn live_spec_change_reaches_next_poll_series_and_movie() {
        let _live_serial = LIVE_SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        config::set_live_card_specs(config::CardSpecs::default());
        let (dc, frx, reader, dir) = live_pipe_harness("live-specs");
        let log = Logger::new(dir.join("live-specs.log"), 0, 1);
        const TOTAL: i64 = 2_832_979;
        const NOW: i64 = 1_000_000_000_000;
        let mut noop = |_: config::CardKind,
                        _: String,
                        _: Option<presence::Action>,
                        _: presence::Resolved| {};
        let mut gate = Gate::new();
        let series_raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
        let act = presence::Action {
            running: true,
            has_start: true,
            start: NOW - 1000,
            ct: 1000,
            ..presence::Action::default()
        };

        // Push once under the startup (default) specs, read through the app's
        // live reader exactly as the poll loop does.
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            series_raw,
            Some(&act),
            1000,
            TOTAL,
            NOW,
            true,
            false,
        );
        let first = frx
            .recv_timeout(Duration::from_secs(2))
            .expect("first SET_ACTIVITY");
        let (d1, s1) = wire_lines(&first);
        let (ed1, es1) = expect_lines(series_raw, &act, &config::CardSpecs::default());
        assert_eq!(d1, ed1);
        assert_eq!(s1.as_deref().unwrap_or(""), es1);

        // A save publishes new specs; the NEXT poll with the same title/ct/total
        // and no event sends the new lines by itself.
        let mut specs_b = config::CardSpecs::default();
        assert!(specs_b.set_line(config::CardKind::Series, config::CardLine::Details, "title"));
        assert!(specs_b.set_line(config::CardKind::Series, config::CardLine::State, "tag"));
        config::set_live_card_specs(specs_b.clone());
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            series_raw,
            Some(&act),
            1000,
            TOTAL,
            NOW + 250,
            false,
            false,
        );
        let second = frx.recv_timeout(Duration::from_secs(2)).expect(
            "a spec change must send on the next poll with no event",
        );
        let (d2, s2) = wire_lines(&second);
        let (ed2, es2) = expect_lines(series_raw, &act, &specs_b);
        assert_eq!(d2, ed2);
        assert_eq!(s2.as_deref().unwrap_or(""), es2);
        assert_ne!((d1, s1), (d2, s2), "the wire must carry the new lines");

        // Movie variant: same-media spec change, so the send can only come from
        // the published specs (the gate's content-change rule), not from a
        // media change or a forced event. First push a MOVIE baseline and
        // record the card the wire carried.
        let movie_raw = "Dune.Part.Two.2024.1080p.BluRay.x264.mkv - PotPlayer";
        let act_m = presence::Action {
            running: true,
            has_start: true,
            start: NOW + 500 - 2000,
            ct: 2000,
            ..presence::Action::default()
        };
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            movie_raw,
            Some(&act_m),
            2000,
            TOTAL,
            NOW + 500,
            true,
            false,
        );
        let movie_first = frx
            .recv_timeout(Duration::from_secs(2))
            .expect("movie baseline SET_ACTIVITY");
        let (dm1, sm1) = wire_lines(&movie_first);
        let (edm1, esm1) = expect_lines(movie_raw, &act_m, &specs_b);
        assert_eq!(dm1, edm1);
        assert_eq!(sm1.as_deref().unwrap_or(""), esm1);

        // A Card-view save publishes a different MOVIE spec (state `none`, so
        // the state field is omitted); the NEXT poll with the SAME title, the
        // same ct/total/action and `event = false` must still send the new
        // movie lines by itself.
        let mut specs_c = specs_b.clone();
        assert!(specs_c.set_line(config::CardKind::Movie, config::CardLine::State, "none"));
        config::set_live_card_specs(specs_c.clone());
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            movie_raw,
            Some(&act_m),
            2000,
            TOTAL,
            NOW + 750,
            false,
            false,
        );
        let third = frx.recv_timeout(Duration::from_secs(2)).expect(
            "a movie spec change must send on the next poll with no event",
        );
        let (d3, s3) = wire_lines(&third);
        let (ed3, es3) = expect_lines(movie_raw, &act_m, &specs_c);
        assert_eq!(d3, ed3);
        assert_eq!(s3.as_deref().unwrap_or(""), es3);
        assert_ne!(
            (dm1, sm1),
            (d3.clone(), s3.clone()),
            "the wire must carry the new movie lines"
        );
        assert!(s3.is_none(), "movie state 'none' must omit the state line");

        config::set_live_card_specs(config::CardSpecs::default());
        let _ = dc.close();
        let _ = reader.join();
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Pinned fact: YES -- while the card is parked paused, a spec change that
    /// alters the composed lines still goes out on the next poll. `gate.rs`
    /// parks only byte-identical paused content (name/details/state/paused/
    /// images/buttons/has_start), so new lines fall through and re-park.
    #[test]
    fn live_spec_change_reaches_paused_card() {
        let _live_serial = LIVE_SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        config::set_live_card_specs(config::CardSpecs::default());
        let (dc, frx, reader, dir) = live_pipe_harness("live-paused");
        let log = Logger::new(dir.join("live-paused.log"), 0, 1);
        const TOTAL: i64 = 2_832_979;
        const NOW: i64 = 1_000_000_000_000;
        let mut noop = |_: config::CardKind,
                        _: String,
                        _: Option<presence::Action>,
                        _: presence::Resolved| {};
        let mut gate = Gate::new();
        let raw = "Show.S01E02.Pilot.1080p.mkv - PotPlayer";
        let paused = presence::Action {
            paused: true,
            ct: 1000,
            ..presence::Action::default()
        };
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            raw,
            Some(&paused),
            1000,
            TOTAL,
            NOW,
            true,
            false,
        );
        let first = frx
            .recv_timeout(Duration::from_secs(2))
            .expect("paused SET_ACTIVITY");
        let (d1, s1) = wire_lines(&first);
        let parked = s1.as_deref().unwrap_or("");
        assert!(
            parked.ends_with(" - Paused"),
            "first frame is the parked pause card, got {parked:?}"
        );
        // A save publishes the Minimal state preset; the next paused poll with
        // the same title/ct/total and no event still sends the new lines.
        let mut specs = config::CardSpecs::default();
        assert!(specs.set_line(config::CardKind::Series, config::CardLine::State, "none"));
        config::set_live_card_specs(specs.clone());
        push_activity(
            &log,
            &dc,
            &mut gate,
            live_card_specs(),
            &mut noop,
            raw,
            Some(&paused),
            1000,
            TOTAL,
            NOW + 250,
            false,
            false,
        );
        let second = frx.recv_timeout(Duration::from_secs(2)).expect(
            "a paused spec change must still go out with no event",
        );
        let (d2, s2) = wire_lines(&second);
        let (ed2, es2) = expect_lines(raw, &paused, &specs);
        assert_eq!(d2, ed2);
        assert_eq!(s2.as_deref().unwrap_or(""), es2);
        assert_eq!(d2, d1, "details unchanged");
        assert!(s2.is_none(), "paused 'none' state must omit the state line");

        config::set_live_card_specs(config::CardSpecs::default());
        let _ = dc.close();
        let _ = reader.join();
        let _ = std::fs::remove_dir_all(&dir);
    }
}

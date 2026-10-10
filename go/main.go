package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"potflix/internal/discord"
	"potflix/internal/metadata"
	"potflix/internal/player"
	"potflix/internal/presence"
)

// Potflix (Go port) - M1 app skeleton + M3 transport/poll loop.
//
// Headless, no console (built with -ldflags "-H=windowsgui"). Argv carries
// only the --selftest switch and the autostart --minimized flag and is
// otherwise ignored.
// Lifecycle, in the Java app's order: single-instance lock first, then the tray
// and the message loop. The poll loop runs beside the tray on its own goroutine.

// Poll-loop cadences (contract section 2).
const (
	pollInterval   = 250 * time.Millisecond // per-poll sleep while tracking
	undefinedSleep = 160 * time.Millisecond // play status Undefined -> re-enumerate
	noPlayerSleep  = 1000 * time.Millisecond
)

const alreadyRunningMsg = "Potflix is already running."

// metaResolver is the single metadata resolver for the poll loop. It is set
// once in runUI before the loop starts (nil in tests, which then keep the
// clean card). Resolve never blocks, so the poll loop never stalls on it;
// a miss or error falls back to today's clean card (zero Resolved).
var metaResolver *metadata.Resolver

// liveConfig is the current settings store. It is loaded once at startup
// and refreshed on every Settings save.
var liveConfig atomic.Pointer[Config]

// previewMu guards previewCard: the EXACT Activity last handed to the
// Discord pipe (the gate-anchored output on a Send verdict). The Card view
// reads this recorded value verbatim - it never rebuilds the payload for
// display (preview-is-the-payload).
var (
	previewMu   sync.Mutex
	previewCard presence.Activity
	previewHave bool
)

// recordPreview stores the exact card handed to SetActivity. It is called
// only on a Send verdict, with the gate-anchored output.
func recordPreview(out presence.Activity) {
	previewMu.Lock()
	previewCard, previewHave = out, true
	previewMu.Unlock()
}

// previewActivity returns the last sent card, or false when nothing was
// sent yet this run (the Card view then shows its waiting placeholder).
func previewActivity() (presence.Activity, bool) {
	previewMu.Lock()
	defer previewMu.Unlock()
	return previewCard, previewHave
}

// metaTitleCache remembers the folder-aware query for the current raw title so
// player.CurrentFile (bounded ~800ms inside) runs at most once per title
// change instead of on every 250 ms poll. A path error or empty path falls
// back cleanly to filename-only inside metadata.ResolveQuery. The poll loop is
// the only writer; the lookup re-resolves PotPlayer via FindFirst because
// pushActivity's frozen signature (see gate_integration_test.go) cannot take
// the tracked hwnd.
var (
	metaLastTitle string
	metaLastQuery metadata.Query
	metaLastSet   bool
)

// metaQueryFor returns the folder-aware query for rawTitle, querying the
// playing-file path once per title change and caching by rawTitle.
func metaQueryFor(rawTitle string) metadata.Query {
	if metaLastSet && rawTitle == metaLastTitle {
		return metaLastQuery
	}
	path := ""
	if hwnd, found := player.FindFirst(); found {
		if p, err := player.CurrentFile(hwnd); err == nil {
			path = p
		}
	}
	q := metadata.ResolveQuery(rawTitle, path)
	metaLastTitle, metaLastQuery, metaLastSet = rawTitle, q, true
	return q
}

func main() {
	// argv is ignored except for the M1 test switch.
	selftest := hasArg(os.Args, "--selftest")
	if selftest {
		attachConsole() // make --selftest output visible from the parent console
	}

	// (1) Single-instance lock: bind 127.0.0.1:17843, backlog 10, held for life.
	// Taken before anything else (Java :61), so a second instance touches no log.
	if err := acquireInstanceLock(); err != nil {
		fmt.Fprintln(os.Stderr, alreadyRunningMsg)
		os.Exit(0)
	}

	// (2) Debug log beside the executable.
	log := newLogger(filepath.Join(exeDir(), "potflix-debug.log"), defaultLogCapBytes, defaultLogMaxFiles)
	defer log.Close()
	appLog = log
	log.logf("instance lock acquired %s:%d backlog=%d", lockHost, lockPort, lockBacklog)

	// (3) Client id: log only the source, never the value.
	_, source := resolveClientID()
	log.logf("client-id source: %s (value not logged)", source)

	if selftest {
		runSelfTest(log)
		return
	}

	runUI(log)
}

// runUI builds the hidden window + tray icon and pumps messages until exit.
func runUI(log *logger) {
	// The window, the notify icon, and the GetMessageW loop must share one
	// OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hwnd, icon, added, err := createTrayWindow()
	if err != nil {
		log.logf("tray init failed: %s", err)
		return
	}
	if !added {
		// Parity with Java :550-551.
		fmt.Fprintln(os.Stderr, "TrayIcon could not be added.")
		log.logf("notify-icon add result=false")
		return
	}
	log.logf("notify-icon add result=true tooltip=Potflix hwnd=0x%x icon=0x%x", hwnd, icon)

	// Tray is up: start the Discord transport and the poll loop on their own
	// goroutines; the message loop below owns this OS thread.
	clientID, _ := resolveClientID()
	dc := discord.New(discord.Config{
		ClientID:  strconv.FormatInt(clientID, 10),
		OnConnect: func() { log.eventConnected() },
		OnReply: func(r discord.Reply) {
			// Discord answers every SET_ACTIVITY; surface its exact verdict.
			if r.Error {
				log.eventPushError(r.Nonce, r.Evt, r.Code, r.Message)
			} else {
				log.eventPushAck(r.Nonce)
			}
		},
		OnAckTimeout: func(nonce string) { log.eventPushAckTimeout(nonce) },
		OnDrop:       func(err error) { log.eventSessionDropped(err) },
	})
	dc.Start()
	cfg := loadConfig()
	liveConfig.Store(&cfg)
	metadata.SetRateProfile(cfg.EffectiveScanSpeed())
	log.logf("scan speed profile: %s", cfg.EffectiveScanSpeed())
	metaResolver = metadata.New(metadata.Options{
		// Default cache dir + keyless sources; Logf carries source
		// names/timeouts only, never secrets.
		Logf: func(format string, args ...any) { log.logf(format, args...) },
	})
	go pollLoop(log, dc)

	// Fleet decision 6 (deviation): clear the Discord card before exiting.
	clearActivity = func() {
		dc.ClearActivity()
		log.logf("cleared (exit)")
	}

	// Settings window: created hidden, shown on manual launch, kept hidden
	// to the tray on the autostart path (Run value carries --minimized).
	// --selftest never reaches here (runSelfTest returns earlier), so it
	// stays headless.
	if err := createMainWindow(); err != nil {
		log.logf("main window init failed: %s", err)
	} else if shouldShowMainWindow(os.Args) {
		showMainWindow()
	} else {
		log.logf("autostart launch: window kept hidden (tray only)")
	}

	messageLoop()
	dc.Close()
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// exeDir is the directory holding the running binary: logs land beside it.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// pollLoop is the contract's pollLoop (section 2). It re-enumerates the first
// PotPlayer window, skips an Undefined play status for 160 ms, then drives the
// presence tracker every 250 ms and pushes every decision to Discord. Window
// gone / title changed clears the card with `cleared (player window gone)`;
// no player window clears once with `cleared (no PotPlayer)`.
func pollLoop(log *logger, dc *discord.Client) {
	// Java starts with cleared=true (Main.java:95): the first no-player pass
	// sends no clear.
	cleared := true
	lastGen := int64(-1)
	var tracker *presence.Tracker
	var gate *presence.Gate

	// A fresh segment (new window, or a reconnect that starts a new Discord
	// session) gets a fresh tracker and gate: the next poll re-emits the card.
	reset := func() {
		tracker = &presence.Tracker{}
		gate = &presence.Gate{}
	}

	// A reconnect starts a fresh Discord session: reset the tracker so the next
	// poll re-emits the current card (contract section 4, "first poll after
	// connect").
	freshSession := func() {
		if gen := dc.Generation(); gen != lastGen {
			lastGen = gen
			reset()
		}
	}

	for {
		freshSession()
		hwnd, found := player.FindFirst()
		if !found {
			if !cleared {
				if err := dc.ClearActivity(); err != nil {
					log.eventSendError(err)
				}
				log.eventClearedNoPlayer()
				cleared = true
			}
			time.Sleep(noPlayerSleep)
			continue
		}
		if player.GetPlayStatus(hwnd) == player.StatusUndefined {
			time.Sleep(undefinedSleep)
			continue
		}
		cleared = false

		// Java makes one PresenceTracker per outer iteration (Main.java:109), so
		// a window that disappears and returns starts a fresh segment rather
		// than inheriting the previous segment's last-seen state.
		reset()

		// Track this window until it dies or stops looking like PotPlayer.
		for player.Alive(hwnd) {
			freshSession()
			title := player.Title(hwnd)
			if !player.Matches(title) {
				break
			}
			st := player.GetPlayStatus(hwnd)
			ct := player.GetCurrentTime(hwnd)
			total := player.GetTotalTime(hwnd)
			now := time.Now().UnixMilli()
			a := tracker.Poll(title, st, ct, now)
			if a != nil {
				log.eventDecision(now, st.String(), ct, title, a.Tag(),
					a.Running, a.Paused, a.HasStart, a.Start, a.Suffix)
			}
			// The tracker yields only on change/seek (or its trailing edge); the
			// gate is consulted EVERY poll with the current card so it can
			// re-anchor on divergence, fire the heartbeat, and log the verdict.
			cur := a
			if cur == nil {
				cur = tracker.Current()
			}
			if cur != nil {
				pushActivity(log, dc, gate, title, cur, ct, total, now, a != nil, tracker.PendingDebounce())
			}
			time.Sleep(pollInterval)
		}

		if err := dc.ClearActivity(); err != nil {
			log.eventSendError(err)
		}
		log.eventClearedWindowGone()
	}
}

// pushActivity builds the card, asks the gate whether it must go out, logs the
// attempt (the observation boundary), and sends only when the gate says so. A
// failed send is not fatal - the client's supervisor reconnects and a later
// decision resends - but it is never silent: the transport error is logged, and
// Discord's own reply (ack or ERROR) is logged by the client's OnReply handler.
func pushActivity(log *logger, dc *discord.Client, gate *presence.Gate, rawTitle string, a *presence.Action, ct, total, now int64, event, debounced bool) {
	card := presence.BuildActivity(rawTitle, a)
	var res presence.Resolved
	if metaResolver != nil {
		// Non-blocking: a miss fires at most one background fetch per key
		// (single-flight inside the resolver) and returns Found=false, so
		// the card below is today's clean card until the fetch lands.
		// Unknown kinds (themes, extras, bare names) never fetch or cache.
		if q := metaQueryFor(rawTitle); q.Kind != metadata.KindUnknown {
			if r, err := metaResolver.Resolve(context.Background(), q); err == nil && r.Found {
				res = presence.Resolved{
					Series: r.Series, EpisodeName: r.Episode, Artwork: r.Artwork, Synopsis: r.Synopsis,
					Genres: r.Genres, Length: r.Length, Year: r.Year, IsMovie: r.IsMovie,
				}
				card = presence.BuildActivityResolved(rawTitle, a, res)
			}
		}
	}
	// Card view spec: the owner's Details/State preset choice. The default
	// spec reproduces BuildActivityResolved byte-for-byte, so a default
	// store skips the second build entirely and the sent card is unchanged.
	if cfg := liveConfig.Load(); cfg != nil {
		if spec := cfg.EffectiveCardSpec(); !spec.IsDefault() {
			card.Details, card.State = spec.Render(rawTitle, a, res)
		}
	}
	out, verdict := gate.Decide(card, ct, total, now, event, debounced)
	log.eventPushAttempt(out, verdict)
	if !verdict.Send {
		return
	}
	// A pause parks the paused card and keeps it, so the client keeps showing
	// the count-UP pause timer; the gate never sets Clear now (Verdict.Clear is
	// retained only for the consumer's SET_ACTIVITY-null path), so this branch
	// is dormant. It stays so a clear decision could still be honored as part of
	// the same send decision, never a separate step that could be debounced or
	// throttled.
	if verdict.Clear {
		if err := dc.ClearActivity(); err != nil {
			log.eventSendError(err)
		}
		return
	}
	log.eventPush(out.Details, out.State, ct, total)
	// Preview-is-the-payload: record the exact value handed to the pipe, so
	// the Card view reads what was SENT, never a rebuilt copy. Recorded only
	// after a successful send: a failed send leaves the previous SENT card
	// in place instead of presenting an unsent attempt as sent.
	if err := dc.SetActivity(discord.Activity{
		Name:       out.Name,
		Details:    out.Details,
		State:      out.State,
		LargeImage: out.LargeImage,
		LargeText:  out.LargeText,
		HasStart:   out.HasStart,
		Start:      out.Start,
		End:        out.End,
	}); err != nil {
		log.eventSendError(err)
	} else {
		recordPreview(out)
	}
}

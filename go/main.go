package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"potflix/internal/discord"
	"potflix/internal/player"
	"potflix/internal/presence"
)

// Potflix (Go port) - M1 app skeleton + M3 transport/poll loop.
//
// Headless, no console (built with -ldflags "-H=windowsgui"), argv ignored.
// Lifecycle, in the Java app's order: single-instance lock first, then the tray
// and the message loop. The poll loop runs beside the tray on its own goroutine.

// Poll-loop cadences (contract section 2).
const (
	pollInterval   = 250 * time.Millisecond // per-poll sleep while tracking
	undefinedSleep = 160 * time.Millisecond // play status Undefined -> re-enumerate
	noPlayerSleep  = 1000 * time.Millisecond
)

const alreadyRunningMsg = "Potflix is already running."

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
	go pollLoop(log, dc)

	// Fleet decision 6 (deviation): clear the Discord card before exiting.
	clearActivity = func() {
		dc.ClearActivity()
		log.logf("cleared (exit)")
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

	// A reconnect starts a fresh Discord session: reset the tracker so the next
	// poll re-emits the current card (contract section 4, "first poll after
	// connect").
	freshSession := func() {
		if gen := dc.Generation(); gen != lastGen {
			lastGen = gen
			tracker = &presence.Tracker{}
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
		tracker = &presence.Tracker{}

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
			if a := tracker.Poll(title, st, ct, now); a != nil {
				log.eventDecision(now, st.String(), ct, title, a.Tag(),
					a.Running, a.Paused, a.HasStart, a.Start, a.Suffix)
				pushActivity(log, dc, title, a, ct, total)
			}
			time.Sleep(pollInterval)
		}

		if err := dc.ClearActivity(); err != nil {
			log.eventSendError(err)
		}
		log.eventClearedWindowGone()
	}
}

// pushActivity logs the contract's `push ...` line and sends the card. A failed
// send is not fatal - the client's supervisor reconnects and a later decision
// resends - but it is never silent: the transport error is logged, and Discord's
// own reply (ack or ERROR) is logged by the client's OnReply handler.
func pushActivity(log *logger, dc *discord.Client, rawTitle string, a *presence.Action, ct, total int64) {
	act := presence.BuildActivity(rawTitle, a)
	log.eventPush(act.Details, act.State, ct, total)
	if err := dc.SetActivity(discord.Activity{
		Details:    act.Details,
		State:      act.State,
		SmallImage: act.SmallImage,
		HasStart:   act.HasStart,
		Start:      act.Start,
	}); err != nil {
		log.eventSendError(err)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"potflix/internal/discord"
	"potflix/internal/player"
	"potflix/internal/presence"
	"potflix/internal/titleclean"
)

// selftest.go - `--selftest`: prove headlessly what M1 can prove.

// attachConsole binds the GUI-subsystem process to the launching console so
// --selftest output is visible. Without it, -H=windowsgui has no stdout.
//
// It only acts when the caller did NOT already provide a stdout handle: if the
// process was launched with a pipe/file redirect (the normal way to capture
// evidence), that handle is kept and left alone.
func attachConsole() {
	if stdHandleValid(stdOutputHandle) {
		return // stdout already redirected; do not steal it
	}
	procAttachConsole.Call(attachParentProcess)
	if stdHandleValid(stdOutputHandle) && stdHandleValid(stdErrorHandle) {
		return // AttachConsole wired handles 1/2 to the parent console
	}
	name, _ := syscall.UTF16PtrFromString("CONOUT$")
	h, _, _ := procCreateFileW.Call(
		uintptr(unsafe.Pointer(name)),
		genericWrite,
		fileShareReadWrite,
		0,
		openExisting,
		0, 0,
	)
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return
	}
	con := os.NewFile(h, "CONOUT$")
	if !stdHandleValid(stdOutputHandle) {
		os.Stdout = con
	}
	if !stdHandleValid(stdErrorHandle) {
		os.Stderr = con
	}
}

func stdHandleValid(n uintptr) bool {
	h, _, _ := procGetStdHandle.Call(n)
	return h != 0 && h != uintptr(syscall.InvalidHandle)
}

func runSelfTest(log *logger) {
	// Mirror everything to a transcript file so the evidence survives even with
	// no console. Transcript first: a failing os.Stdout write then cannot stop it.
	transcript := filepath.Join(exeDir(), "m1-selftest.out")
	tf, terr := os.Create(transcript)
	var out io.Writer = os.Stdout
	if terr == nil {
		out = io.MultiWriter(tf, os.Stdout)
	}
	finish := func() {
		if tf != nil {
			tf.Sync()
			tf.Close()
		}
	}
	fail := func(format string, a ...any) {
		fmt.Fprintf(out, "M1 SELFTEST FAIL: "+format+"\n", a...)
		finish()
		os.Exit(1)
	}
	ok := func(format string, a ...any) {
		fmt.Fprintf(out, format+"\n", a...)
	}
	_ = terr

	// (0) struct layout sanity: NOTIFYICONDATAW is 976 bytes on amd64. If this
	// is wrong, Shell_NotifyIconW may reject cbSize.
	if n := unsafe.Sizeof(notifyIconData{}); n != 976 {
		fail("NOTIFYICONDATAW size = %d, want 976", n)
	}
	ok("struct layout: NOTIFYICONDATAW=%d bytes", unsafe.Sizeof(notifyIconData{}))

	// (1) client-id resolution: source only, never the value.
	_, source := resolveClientID()
	ok("client-id source: %s (value not logged)", source)
	log.logf("selftest: client-id source=%s", source)

	// (2) hidden window + embedded icon + notify icon.
	runtime.LockOSThread()
	hwnd, icon, added, err := createTrayWindow()
	if err != nil {
		fail("tray window init: %v", err)
	}
	ok("hidden window created: hwnd=0x%x", hwnd)
	if icon == 0 {
		_, derr := loadEmbeddedIconDiag()
		fail("embedded icon did not load: %v", derr)
	}
	ok("embedded icon loaded: hicon=0x%x", icon)
	if !added {
		fail("Shell_NotifyIconW NIM_ADD returned false")
	}
	ok("notify-icon add returned true")
	log.logf("selftest: notify-icon add result=true")

	// (3) pump the message loop for ~3 s.
	time.AfterFunc(3*time.Second, func() {
		procPostMessageW.Call(hwnd, wmAppQuit, 0, 0)
	})
	start := time.Now()
	messageLoop()
	elapsed := time.Since(start)
	if elapsed < 2500*time.Millisecond {
		fail("message loop exited early after %v", elapsed)
	}
	ok("message loop pumped %v", elapsed.Round(time.Millisecond))

	trayDelete(hwnd)
	procDestroyWindow.Call(hwnd)
	runtime.UnlockOSThread()

	// (4) debug log is written, and rotation works at a forced tiny cap.
	log.logf("selftest: rotation check starting")
	if st, err := os.Stat(log.path); err != nil {
		fail("debug log not written: %v", err)
	} else if st.Size() == 0 {
		fail("debug log is empty")
	}
	rotPath := filepath.Join(exeDir(), "potflix-debug-selftest.log")
	for _, suffix := range []string{"", ".1", ".2"} {
		os.Remove(rotPath + suffix)
	}
	rot := newLogger(rotPath, 256, 3)
	for i := 0; i < 60; i++ {
		rot.logf("rotation test line %03d padding padding padding", i)
	}
	rot.Close()
	if _, err := os.Stat(rotPath + ".1"); err != nil {
		fail("rotation did not produce %s.1: %v", rotPath, err)
	}
	st, err := os.Stat(rotPath)
	if err != nil {
		fail("rotated current log missing: %v", err)
	}
	if st.Size() > 256 {
		fail("rotated current log %d bytes exceeds cap 256", st.Size())
	}
	ok("log written: %s; rotation at cap 256 -> .1 present, current=%d bytes", log.path, st.Size())
	log.logf("selftest: rotation ok (.1 present, current=%d bytes)", st.Size())

	// (5) a second instance prints the exact line and exits 0.
	exe, err := os.Executable()
	if err != nil {
		fail("os.Executable: %v", err)
	}
	cmd := exec.Command(exe)
	var captured bytes.Buffer
	cmd.Stdout = &captured
	cmd.Stderr = &captured
	runErr := cmd.Run()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if runErr != nil {
		fail("second instance did not run: %v", runErr)
	}
	got := strings.TrimSpace(captured.String())
	if got != alreadyRunningMsg {
		fail("second instance stderr=%q, want %q (runErr=%v)", got, alreadyRunningMsg, runErr)
	}
	if exitCode != 0 {
		fail("second instance exit=%d, want 0", exitCode)
	}
	ok("second instance: stderr=%q exit=%d", got, exitCode)
	log.logf("selftest: second instance printed %q and exited %d", got, exitCode)

	// (6) titleclean: the public surface (integration of the proven cleaner).
	c := titleclean.Clean("Show.S01E02.Pilot.1080p.mkv")
	if c.Title != "Show" || c.EpisodeTag != "S1 E2" || c.EpisodeName != "Pilot" {
		fail("titleclean.Clean = %+v, want title=Show tag=\"S1 E2\" name=Pilot", c)
	}
	if got := titleclean.Trunc("abcdef", 4); got != "abc\u2026" {
		fail("titleclean.Trunc(abcdef,4) = %q, want abc<U+2026>", got)
	}
	ok("titleclean: Clean -> title=%q tag=%q name=%q; Trunc ok", c.Title, c.EpisodeTag, c.EpisodeName)

	// (7) player: the title predicate and the status mapping (pure logic).
	for _, tc := range []struct {
		title string
		want  bool
	}{
		{"Some Movie - PotPlayer", true},
		{"PotPlayer", true},
		{"Some Movie", false},
		{"- PotPlayer", false},
	} {
		if got := player.Matches(tc.title); got != tc.want {
			fail("player.Matches(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
	for _, tc := range []struct {
		raw  int
		want player.PlayStatus
	}{
		{-1, player.StatusStopped}, {1, player.StatusPaused}, {2, player.StatusRunning},
		{0, player.StatusUndefined}, {3, player.StatusUndefined},
	} {
		if got := player.MapPlayStatus(tc.raw); got != tc.want {
			fail("player.MapPlayStatus(%d) = %s, want %s", tc.raw, got, tc.want)
		}
	}
	ok("player: title predicate and status mapping ok")

	// (8) presence: run a contract transition matrix and write the decision log
	// through the real event writer.
	matrix := []struct {
		title  string
		status player.PlayStatus
		ct     int64
		now    int64
	}{
		{"A - PotPlayer", player.StatusRunning, 1000, 100000},
		{"A - PotPlayer", player.StatusRunning, 1250, 100250},
		{"A - PotPlayer", player.StatusPaused, 1250, 100500},
		{"A - PotPlayer", player.StatusPaused, 1250, 100750},
		{"A - PotPlayer", player.StatusRunning, 1260, 101000},
		{"A - PotPlayer", player.StatusStopped, 0, 101250},
	}
	tr := &presence.Tracker{}
	emits, lastTag := 0, ""
	for _, s := range matrix {
		a := tr.Poll(s.title, s.status, s.ct, s.now)
		if a == nil {
			continue
		}
		emits++
		lastTag = a.Tag()
		log.eventDecision(s.now, s.status.String(), s.ct, s.title, a.Tag(), a.Running, a.Paused, a.HasStart, a.Start, a.Suffix)
		ok("  decision ms=%d st=%s ct=%d action=%s", s.now, s.status, s.ct, a.Tag())
	}
	if emits != 4 || lastTag != "STOP" {
		fail("presence matrix: emits=%d lastTag=%q, want 4 and STOP", emits, lastTag)
	}
	ok("presence: transition matrix produced %d decisions, last=%s", emits, lastTag)

	// (9) presence: payload helpers.
	if got := presence.WithStateTime("S1 E2", " - 01:23"); got != "S1 E2 - 01:23" {
		fail("presence.WithStateTime = %q, want \"S1 E2 - 01:23\"", got)
	}
	if got := presence.WithStateTime("", " - 01:23"); got != "01:23" {
		fail("presence.WithStateTime leading-dash strip = %q, want 01:23", got)
	}
	if got := presence.Fmt(3661000); got != "1:01:01" {
		fail("presence.Fmt(3661000) = %q, want 1:01:01", got)
	}
	ok("presence: WithStateTime and Fmt ok")

	// (10) fleet decision 10: a repeated RUN seek within ~1 s is debounced.
	dtr := &presence.Tracker{}
	dtr.Poll("D - PotPlayer", player.StatusRunning, 1000, 0)
	if a := dtr.Poll("D - PotPlayer", player.StatusRunning, 5000, 200); a != nil {
		fail("debounce: seek within 1 s should be suppressed, got %+v", a)
	}
	ok("presence: RUN debounce suppressed a repeat within 1 s")

	// (11) live PotPlayer window: report, never fail, when absent.
	if hwnd, found := player.FindFirst(); found {
		title := player.Title(hwnd)
		st := player.GetPlayStatus(hwnd)
		ok("player: live PotPlayer hwnd=0x%x title=[%s] alive=%t status=%s ct=%d total=%d",
			hwnd, title, player.Alive(hwnd), st, player.GetCurrentTime(hwnd), player.GetTotalTime(hwnd))
		log.logf("selftest: live PotPlayer hwnd=0x%x title=[%s] status=%s", hwnd, title, st)
	} else {
		ok("player: no PotPlayer window found; live control/title read deferred")
	}

	// (12) M3: the Discord IPC transport, proven end to end against the
	// in-process fake pipe. This never touches the real \\.\pipe\discord-ipc-*
	// namespace: the server binds a process-unique \\.\pipe\potflix-fake-* name.
	m3fail := func(format string, a ...any) {
		fmt.Fprintf(out, "M3 SELFTEST FAIL: "+format+"\n", a...)
		finish()
		os.Exit(1)
	}

	srv := discord.NewFakeServer(0)
	if !strings.HasPrefix(srv.Name(), `\\.\pipe\potflix-fake-`) {
		m3fail("fake pipe %q is outside the potflix-fake-* namespace", srv.Name())
	}
	if strings.Contains(srv.Name(), "discord-ipc") {
		m3fail("fake pipe %q collides with the real discord-ipc namespace", srv.Name())
	}

	const m3ID = "123456789012345678"
	drops := make(chan error, 4)
	client := discord.New(discord.Config{
		ClientID:   m3ID,
		PipeBase:   srv.Base,
		RetryDelay: 50 * time.Millisecond,
		OnDrop: func(err error) {
			select {
			case drops <- err:
			default:
			}
		},
	})

	// accept waits (bounded) for the next client connection on the fake pipe.
	accept := func() (*discord.FakeConn, error) {
		ch := make(chan *discord.FakeConn, 1)
		ech := make(chan error, 1)
		go func() {
			sc, err := srv.Accept()
			if err != nil {
				ech <- err
				return
			}
			ch <- sc
		}()
		select {
		case sc := <-ch:
			return sc, nil
		case err := <-ech:
			return nil, err
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("timed out waiting for the fake-pipe client")
		}
	}

	client.Start()
	defer client.Close()

	sc, err := accept()
	if err != nil {
		m3fail("connect: %v", err)
	}
	defer sc.Close()

	// (12a) handshake: first frame is opcode 0 carrying the client hello.
	op, hs, err := sc.ReadFrame()
	if err != nil {
		m3fail("read handshake: %v", err)
	}
	if op != discord.OpHandshake {
		m3fail("first frame opcode = %d, want %d (handshake)", op, discord.OpHandshake)
	}
	var hello struct {
		V        int    `json:"v"`
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(hs, &hello); err != nil {
		m3fail("handshake not JSON: %s (%v)", hs, err)
	}
	if hello.V != 1 || hello.ClientID != m3ID {
		m3fail("handshake = %s, want v=1 client_id=%s", hs, m3ID)
	}
	// Discord answers the handshake with a READY dispatch; the client refuses to
	// send commands until it has seen READY (Discord discards earlier commands).
	if err := sc.WriteFrame(discord.OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		m3fail("write READY: %v", err)
	}
	waitReady := func(want int64, what string) {
		deadline := time.Now().Add(2 * time.Second)
		for client.Generation() < want {
			if time.Now().After(deadline) {
				m3fail("client never became ready %s (generation %d, want %d)", what, client.Generation(), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	waitReady(1, "after the READY dispatch")
	ok("m3: handshake %s; READY -> generation %d", hs, client.Generation())
	log.logf("selftest: m3 handshake + READY ok")

	// (12b) SET_ACTIVITY: command envelope + activity object + assets + timer.
	if err := client.SetActivity(discord.Activity{
		Details: "Show", State: `S1 E2 "Pilot"`, SmallImage: "play", HasStart: true, Start: 12345,
	}); err != nil {
		m3fail("SetActivity: %v", err)
	}
	op, payload, err := sc.ReadFrame()
	if err != nil {
		m3fail("read activity: %v", err)
	}
	if op != discord.OpFrame {
		m3fail("activity opcode = %d, want %d", op, discord.OpFrame)
	}
	var setCmd struct {
		Cmd   string `json:"cmd"`
		Nonce string `json:"nonce"`
		Args  struct {
			PID      int `json:"pid"`
			Activity *struct {
				Type       int    `json:"type"`
				Details    string `json:"details"`
				State      string `json:"state"`
				Timestamps *struct {
					Start int64 `json:"start"`
				} `json:"timestamps"`
				Assets map[string]any `json:"assets"`
			} `json:"activity"`
		} `json:"args"`
	}
	if err := json.Unmarshal(payload, &setCmd); err != nil {
		m3fail("activity not JSON: %s (%v)", payload, err)
	}
	if setCmd.Cmd != "SET_ACTIVITY" {
		m3fail("cmd = %q, want SET_ACTIVITY", setCmd.Cmd)
	}
	if setCmd.Nonce == "" {
		m3fail("nonce empty in %s", payload)
	}
	if setCmd.Args.PID != os.Getpid() {
		m3fail("pid = %d, want %d", setCmd.Args.PID, os.Getpid())
	}
	a := setCmd.Args.Activity
	if a == nil {
		m3fail("activity null, want an object")
	}
	if a.Type != 0 || a.Details != "Show" || a.State != `S1 E2 "Pilot"` {
		m3fail("activity = %+v", a)
	}
	if a.Timestamps == nil || a.Timestamps.Start != 12345 {
		m3fail("timestamps = %+v, want start=12345", a.Timestamps)
	}
	// Empty asset text is omitted on the wire (Discord rejects empty
	// large_text/small_text with code 4000).
	if a.Assets["large_image"] != "logo" || a.Assets["small_image"] != "play" {
		m3fail("assets = %v", a.Assets)
	}
	if _, ok := a.Assets["large_text"]; ok {
		m3fail("large_text present while empty, want omitted: %v", a.Assets)
	}
	if _, ok := a.Assets["small_text"]; ok {
		m3fail("small_text present while empty, want omitted: %v", a.Assets)
	}
	ok("m3: SET_ACTIVITY pid=%d details=%q small_image=%q start=%d", setCmd.Args.PID, a.Details, a.Assets["small_image"], a.Timestamps.Start)
	log.logf("selftest: m3 set_activity ok")

	// (12c) null clear: SET_ACTIVITY with "activity":null.
	if err := client.ClearActivity(); err != nil {
		m3fail("ClearActivity: %v", err)
	}
	op, payload, err = sc.ReadFrame()
	if err != nil {
		m3fail("read clear: %v", err)
	}
	if op != discord.OpFrame {
		m3fail("clear opcode = %d, want %d", op, discord.OpFrame)
	}
	var clearTop struct {
		Cmd  string         `json:"cmd"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(payload, &clearTop); err != nil {
		m3fail("clear not JSON: %s (%v)", payload, err)
	}
	if clearTop.Cmd != "SET_ACTIVITY" {
		m3fail("clear cmd = %q, want SET_ACTIVITY", clearTop.Cmd)
	}
	if clearTop.Args["activity"] != nil {
		m3fail("clear activity = %v, want null", clearTop.Args["activity"])
	}
	ok("m3: null clear SET_ACTIVITY activity=null")
	log.logf("selftest: m3 null clear ok")

	// (12d) PING -> PONG: the read pump answers a server ping on the wire.
	if err := sc.WriteFrame(discord.OpPing, []byte("selftest-ping")); err != nil {
		m3fail("write ping: %v", err)
	}
	op, payload, err = sc.ReadFrame()
	if err != nil {
		m3fail("read pong: %v", err)
	}
	if op != discord.OpPong || string(payload) != "selftest-ping" {
		m3fail("pong = op %d payload %q, want op %d %q", op, payload, discord.OpPong, "selftest-ping")
	}
	ok("m3: ping answered opcode=%d payload=%q", op, payload)
	log.logf("selftest: m3 ping/pong ok")

	// (12e) reconnect: a dropped transport re-handshakes, OnDrop fires, and the
	// generation advances so the poll loop re-emits the current card.
	genBefore := client.Generation()
	sc.Close() // drop the server side; the client must reconnect
	sc, err = accept()
	if err != nil {
		m3fail("reconnect accept: %v", err)
	}
	defer sc.Close()
	op, hs, err = sc.ReadFrame()
	if err != nil {
		m3fail("read reconnect handshake: %v", err)
	}
	if op != discord.OpHandshake {
		m3fail("reconnect first frame opcode = %d, want %d", op, discord.OpHandshake)
	}
	if err := json.Unmarshal(hs, &hello); err != nil || hello.ClientID != m3ID {
		m3fail("reconnect handshake = %s (err=%v)", hs, err)
	}
	// The reconnect must also reach READY before commands are allowed.
	if err := sc.WriteFrame(discord.OpFrame, []byte(`{"cmd":"DISPATCH","evt":"READY","data":{"v":1}}`)); err != nil {
		m3fail("write reconnect READY: %v", err)
	}
	waitReady(genBefore+1, "after reconnect READY")
	select {
	case <-drops:
	case <-time.After(5 * time.Second):
		m3fail("OnDrop was not called after the fake server dropped the pipe")
	}
	genAfter := client.Generation()
	if genAfter <= genBefore {
		m3fail("generation did not advance on reconnect: before=%d after=%d", genBefore, genAfter)
	}
	ok("m3: reconnect handshake ok, generation %d -> %d", genBefore, genAfter)
	log.logf("selftest: m3 reconnect ok generation=%d->%d", genBefore, genAfter)

	fmt.Fprintln(out, "M3 LAYER SELFTEST PASS")
	fmt.Fprintln(out, "M2 LAYER SELFTEST PASS")
	fmt.Fprintln(out, "M1 SELFTEST PASS")
	finish()
	os.Exit(0)
}

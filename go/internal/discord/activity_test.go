package discord

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// activity_test.go - the exact SET_ACTIVITY payload shape (contract section 5):
// entity fields, no timestamps when there is no timer, and a null-activity
// clear. Assertions check the key set explicitly, so a stray field fails.

func decodeFrame(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", payload, err)
	}
	return m
}

func wantKeys(t *testing.T, where string, m map[string]any, keys ...string) {
	t.Helper()
	if len(m) != len(keys) {
		t.Fatalf("%s keys = %v, want exactly %v", where, keysOf(m), keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s missing key %q (has %v)", where, k, keysOf(m))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func asMap(t *testing.T, v any, where string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want object", where, v)
	}
	return m
}

func TestSetActivityPayload(t *testing.T) {
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	act := Activity{Details: "Show", State: `S1 E2 "Pilot"`, SmallImage: "play", HasStart: true, Start: 12345}
	if err := c.SetActivity(act); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	op, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read activity frame: %v", err)
	}
	if op != OpFrame {
		t.Fatalf("activity opcode = %d, want %d", op, OpFrame)
	}

	top := decodeFrame(t, payload)
	wantKeys(t, "command", top, "cmd", "args", "nonce")
	if top["cmd"] != "SET_ACTIVITY" {
		t.Fatalf("cmd = %v, want SET_ACTIVITY", top["cmd"])
	}
	if nonce, _ := top["nonce"].(string); nonce == "" {
		t.Fatalf("nonce missing in %s", payload)
	}

	args := asMap(t, top["args"], "args")
	wantKeys(t, "args", args, "pid", "activity")
	if got := int(args["pid"].(float64)); got != os.Getpid() {
		t.Fatalf("pid = %d, want %d", got, os.Getpid())
	}

	a := asMap(t, args["activity"], "activity")
	wantKeys(t, "activity", a, "type", "details", "state", "timestamps", "assets")
	if a["type"].(float64) != 0 {
		t.Fatalf("type = %v, want 0 (PLAYING)", a["type"])
	}
	if a["details"] != "Show" {
		t.Fatalf("details = %v, want Show", a["details"])
	}
	if a["state"] != `S1 E2 "Pilot"` {
		t.Fatalf("state = %v, want %s", a["state"], `S1 E2 "Pilot"`)
	}

	ts := asMap(t, a["timestamps"], "timestamps")
	wantKeys(t, "timestamps", ts, "start")
	if ts["start"].(float64) != 12345 {
		t.Fatalf("timestamps.start = %v, want 12345", ts["start"])
	}

	// Empty text is OMITTED, not sent as "" (Discord rejects an empty
	// large_text with code 4000); non-empty asset fields stay present.
	assets := asMap(t, a["assets"], "assets")
	wantKeys(t, "assets", assets, "large_image", "small_image")
	if assets["large_image"] != "logo" || assets["small_image"] != "play" {
		t.Fatalf("assets = %v", assets)
	}
	if _, ok := assets["large_text"]; ok {
		t.Fatalf("large_text present while empty, want omitted: %v", assets)
	}
	if _, ok := assets["small_text"]; ok {
		t.Fatalf("small_text present while empty, want omitted: %v", assets)
	}
}

func TestSetActivityTextPresentWhenNonEmpty(t *testing.T) {
	// The wire object omits empty text but must still carry non-empty text when a
	// caller sets it.
	p := newActivity(Activity{Details: "Show", State: "S1 E2", SmallImage: "play"})
	p.Assets.LargeText = "Potflix"
	p.Assets.SmallText = "playing"
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	a := decodeFrame(t, b)
	assets := asMap(t, a["assets"], "assets")
	wantKeys(t, "assets", assets, "large_image", "large_text", "small_image", "small_text")
	if assets["large_text"] != "Potflix" || assets["small_text"] != "playing" {
		t.Fatalf("assets = %v", assets)
	}
}

func TestSetActivityNoTimer(t *testing.T) {
	// The idle card: details Browsing / state Idle / small_image stop, and no
	// timestamps object at all.
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := c.SetActivity(Activity{Details: "Browsing", State: "Idle", SmallImage: "stop"}); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	_, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read activity frame: %v", err)
	}
	top := decodeFrame(t, payload)
	a := asMap(t, asMap(t, top["args"], "args")["activity"], "activity")
	wantKeys(t, "activity", a, "type", "details", "state", "assets")
	if a["details"] != "Browsing" || a["state"] != "Idle" {
		t.Fatalf("idle card = %v", a)
	}
	assets := asMap(t, a["assets"], "assets")
	wantKeys(t, "assets", assets, "large_image", "small_image")
	if assets["small_image"] != "stop" {
		t.Fatalf("idle small_image = %v, want stop", assets["small_image"])
	}
	if _, ok := assets["large_text"]; ok {
		t.Fatalf("large_text present while empty, want omitted: %v", assets)
	}
	if _, ok := assets["small_text"]; ok {
		t.Fatalf("small_text present while empty, want omitted: %v", assets)
	}
}

func TestSetActivityEndPresentWhenKnown(t *testing.T) {
	// README deviation 5: while running and the duration is known, the wire
	// carries both timestamps.start and timestamps.end so Discord extrapolates
	// between pushes.
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := c.SetActivity(Activity{Details: "Show", State: "S1 E2", SmallImage: "play", HasStart: true, Start: 111, End: 222}); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	_, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read activity frame: %v", err)
	}
	top := decodeFrame(t, payload)
	a := asMap(t, asMap(t, top["args"], "args")["activity"], "activity")
	ts := asMap(t, a["timestamps"], "timestamps")
	wantKeys(t, "timestamps", ts, "start", "end")
	if ts["start"].(float64) != 111 || ts["end"].(float64) != 222 {
		t.Fatalf("timestamps = %v, want start=111 end=222", ts)
	}
}

func TestSetActivityEndOmittedWhenUnknown(t *testing.T) {
	// total unknown (End 0): end is omitted, start still sent.
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := c.SetActivity(Activity{Details: "Show", State: "S1 E2", SmallImage: "play", HasStart: true, Start: 111}); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	_, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read activity frame: %v", err)
	}
	top := decodeFrame(t, payload)
	a := asMap(t, asMap(t, top["args"], "args")["activity"], "activity")
	ts := asMap(t, a["timestamps"], "timestamps")
	wantKeys(t, "timestamps", ts, "start")
}

func TestClearActivityIsNull(t *testing.T) {
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := c.ClearActivity(); err != nil {
		t.Fatalf("ClearActivity: %v", err)
	}
	op, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read clear frame: %v", err)
	}
	if op != OpFrame {
		t.Fatalf("clear opcode = %d, want %d", op, OpFrame)
	}
	if !strings.Contains(string(payload), `"activity":null`) {
		t.Fatalf("clear payload = %s, want activity null", payload)
	}
	top := decodeFrame(t, payload)
	wantKeys(t, "command", top, "cmd", "args", "nonce")
	args := asMap(t, top["args"], "args")
	wantKeys(t, "args", args, "pid", "activity")
	if args["activity"] != nil {
		t.Fatalf("activity = %v, want null", args["activity"])
	}
}

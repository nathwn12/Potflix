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
	if a["type"].(float64) != 3 {
		t.Fatalf("type = %v, want 3 (WATCHING)", a["type"])
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

func TestLargeImageHTTPSHonoredVerbatim(t *testing.T) {
	// An https LargeImage is honored verbatim in the wire JSON.
	const art = "https://static.tvmaze.com/uploads/images/medium_portrait/1/2.jpg"
	got := WireJSON(Activity{Details: "Show", State: "S1 E2", LargeImage: art, SmallImage: "play"})
	want := `{"type":3,"details":"Show","state":"S1 E2","assets":{"large_image":"` + art + `","small_image":"play"}}`
	if got != want {
		t.Fatalf("WireJSON = %s, want %s", got, want)
	}
}

func TestLargeImageFallbackToLogo(t *testing.T) {
	// Empty or non-http(s) LargeImage falls back to "logo", never empty.
	for _, img := range []string{"", "logo", "not-a-url", "ftp://example.com/a.jpg", "data:image/png;base64,xx"} {
		got := WireJSON(Activity{Details: "Show", SmallImage: "play", LargeImage: img})
		a := decodeFrame(t, []byte(got))
		assets := asMap(t, a["assets"], "assets")
		if assets["large_image"] != "logo" {
			t.Fatalf("LargeImage %q: large_image = %v, want logo", img, assets["large_image"])
		}
	}
	// http (non-s) is also honored verbatim.
	const httpArt = "http://example.com/cover.jpg"
	got := WireJSON(Activity{Details: "Show", SmallImage: "play", LargeImage: httpArt})
	a := decodeFrame(t, []byte(got))
	if asMap(t, a["assets"], "assets")["large_image"] != httpArt {
		t.Fatalf("http LargeImage not honored: %s", got)
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

func TestSetActivityPlayingCarriesStartAndEnd(t *testing.T) {
	// While playing the payload carries BOTH timestamps.start and
	// timestamps.end (the remaining-time math is computed by the send gate:
	// end = now + (total - pos)), so Discord renders the time bar against the
	// real media position.
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
	if a["type"].(float64) != 3 {
		t.Fatalf("type = %v, want 3 (WATCHING)", a["type"])
	}
	ts := asMap(t, a["timestamps"], "timestamps")
	wantKeys(t, "timestamps", ts, "start", "end")
	if ts["start"].(float64) != 111 {
		t.Fatalf("timestamps.start = %v, want 111", ts["start"])
	}
	if ts["end"].(float64) != 222 {
		t.Fatalf("timestamps.end = %v, want 222", ts["end"])
	}
}

func TestSetActivityPausedStartOnlyNoEnd(t *testing.T) {
	// A paused card carries timestamps.start anchored at the pause moment and NO
	// timestamps.end, so the client counts the pause UP ("paused for M:SS").
	c, sc, _ := connectPair(t, 0, Config{})
	defer sc.Close()

	if err := c.SetActivity(Activity{Details: "Show", State: `S1 E2 "Pilot" - Paused`, SmallImage: "pause", HasStart: true, Start: 5000}); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}
	_, payload, err := sc.ReadFrame()
	if err != nil {
		t.Fatalf("read activity frame: %v", err)
	}
	if strings.Contains(string(payload), `"end"`) {
		t.Fatalf("paused payload carries end: %s", payload)
	}
	top := decodeFrame(t, payload)
	a := asMap(t, asMap(t, top["args"], "args")["activity"], "activity")
	if a["type"].(float64) != 3 {
		t.Fatalf("type = %v, want 3 (WATCHING)", a["type"])
	}
	ts := asMap(t, a["timestamps"], "timestamps")
	wantKeys(t, "timestamps", ts, "start")
	if ts["start"].(float64) != 5000 {
		t.Fatalf("timestamps.start = %v, want 5000", ts["start"])
	}
}

func TestWireJSONMapping(t *testing.T) {
	// The Netflix-style card on the exact wire object: header `name`, two body
	// lines (`details`, `state`), brand texts, and no buttons/party/secrets.
	cases := []struct {
		name string
		act  Activity
		want string
	}{
		{
			name: "movie",
			act:  Activity{Name: "Movie", Details: "Movie", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing", HasStart: true, Start: 12345, End: 67890},
			want: `{"type":3,"name":"Movie","details":"Movie","timestamps":{"start":12345,"end":67890},"assets":{"large_image":"logo","large_text":"PotFlix","small_image":"play","small_text":"Playing"}}`,
		},
		{
			name: "tv with episode name and tag",
			act:  Activity{Name: "Show", Details: "Pilot", State: "S1 E2", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing", HasStart: true, Start: 111, End: 222},
			want: `{"type":3,"name":"Show","details":"Pilot","state":"S1 E2","timestamps":{"start":111,"end":222},"assets":{"large_image":"logo","large_text":"PotFlix","small_image":"play","small_text":"Playing"}}`,
		},
		{
			name: "tv with tag only",
			act:  Activity{Name: "Show", Details: "S1 E2", LargeText: "PotFlix", SmallImage: "play", SmallText: "Playing", HasStart: true, Start: 111, End: 222},
			want: `{"type":3,"name":"Show","details":"S1 E2","timestamps":{"start":111,"end":222},"assets":{"large_image":"logo","large_text":"PotFlix","small_image":"play","small_text":"Playing"}}`,
		},
		{
			name: "paused",
			act:  Activity{Name: "Show", Details: "Pilot", State: "S1 E2 - Paused", LargeText: "PotFlix", SmallImage: "pause", SmallText: "Paused", HasStart: true, Start: 5000},
			want: `{"type":3,"name":"Show","details":"Pilot","state":"S1 E2 - Paused","timestamps":{"start":5000},"assets":{"large_image":"logo","large_text":"PotFlix","small_image":"pause","small_text":"Paused"}}`,
		},
		{
			name: "idle",
			act:  Activity{Details: "Browsing", State: "Idle", LargeText: "PotFlix", SmallImage: "stop", SmallText: "Idle"},
			want: `{"type":3,"details":"Browsing","state":"Idle","assets":{"large_image":"logo","large_text":"PotFlix","small_image":"stop","small_text":"Idle"}}`,
		},
	}
	for _, c := range cases {
		if got := WireJSON(c.act); got != c.want {
			t.Errorf("%s: WireJSON = %s, want %s", c.name, got, c.want)
		}
	}
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

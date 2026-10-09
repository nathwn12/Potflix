package presence

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// cardspec_test.go - proves the Card spec seam stays safe: the default spec
// renders BYTE-FOR-BYTE what BuildActivityResolved produces today, the zero
// value falls back to it, and every preset stays within the 128-unit cap
// with the paused marker preserved.

type cardSpecCase struct {
	name string
	raw  string
	act  *Action
	res  Resolved
}

func cardSpecCases() []cardSpecCase {
	return []cardSpecCase{
		{"idle-empty", "", nil, Resolved{}},
		{"idle-app", "PotPlayer", &Action{}, Resolved{}},
		{"series-run", "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 12345, CT: 1000}, Resolved{}},
		{"series-tagonly", "Show.S01E02.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 12345, CT: 1000}, Resolved{}},
		{"series-paused", "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
			&Action{Paused: true, CT: 1000}, Resolved{}},
		{"series-resolved", "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			Resolved{Series: "Canonical Series", EpisodeName: "Canonical Episode", Synopsis: "A brave synopsis"}},
		{"series-resolved-paused", "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
			&Action{Paused: true, CT: 1000},
			Resolved{Series: "Canonical Series", EpisodeName: "Canonical Episode", Synopsis: "A brave synopsis"}},
		{"series-synonly", "Movie.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 1, CT: 2},
			Resolved{Synopsis: "Just a synopsis"}},
		{"movie-full", "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
			Resolved{Series: "Dune: Part Two", Year: 2024, IsMovie: true,
				Genres: "Sci-Fi", Length: "2h 46m", Synopsis: "Paul unites the Fremen"}},
		{"movie-paused", "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
			&Action{Paused: true, CT: 1000},
			Resolved{Series: "Dune", Year: 2024, IsMovie: true,
				Genres: "Sci-Fi", Length: "2h 46m", Synopsis: "Paul unites the Fremen"}},
		{"movie-noyear", "Dune.Part.Two.2024.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 1, CT: 2},
			Resolved{Series: "Dune: Part Two", IsMovie: true}},
		{"artwork-buttons", "Show.S01E02.Pilot.1080p.mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 1, CT: 2},
			Resolved{Series: "S", Artwork: "https://example.com/poster.jpg",
				Buttons: []Button{{Label: "Trailer", URL: "https://example.com/trailer"}}}},
		{"long", strings.Repeat("L", 200) + ".S01E02." + strings.Repeat("E", 200) + ".mkv - PotPlayer",
			&Action{Running: true, HasStart: true, Start: 1, CT: 2},
			Resolved{Series: strings.Repeat("y", 200), EpisodeName: strings.Repeat("z", 200),
				Synopsis: strings.Repeat("word ", 40) + "tail"}},
		{"stop", "Show.S01E02.Pilot.1080p.mkv - PotPlayer", &Action{CT: 5}, Resolved{}},
		{"nil-action", "Show.S01E02.Pilot.1080p.mkv - PotPlayer", nil, Resolved{}},
	}
}

// TestCardSpecDefaultEquivalence is the safety gate for the Card view: for a
// table of inputs, DefaultCardSpec().Render must equal what
// presence.BuildActivityResolved (presence.go:499) produces today.
// NOTE: this is a change-detector only (Render calls BuildActivityResolved
// for the default case), so preview-is-the-payload is proven separately by
// TestPreviewIsPayloadThroughSend in go/cardview_test.go, which captures the
// real wire payload through pushActivity and asserts the preview equals it.
func TestCardSpecDefaultEquivalence(t *testing.T) {
	def := DefaultCardSpec()
	for _, c := range cardSpecCases() {
		want := BuildActivityResolved(c.raw, c.act, c.res)
		gotD, gotS := def.Render(c.raw, c.act, c.res)
		if gotD != want.Details || gotS != want.State {
			t.Errorf("%s: Render = (%q, %q), want (%q, %q)",
				c.name, gotD, gotS, want.Details, want.State)
		}
	}
}

// TestCardSpecDefaultGoldenIndependent breaks the tautology: instead of
// comparing Render to BuildActivityResolved (which Render calls for the
// default case), it asserts hardcoded golden lines computed independently.
// If Render drifts from the shipped output, this fails even though the
// change-detector above would still pass. Negative control verified
// (changing any golden breaks this test).
func TestCardSpecDefaultGoldenIndependent(t *testing.T) {
	def := DefaultCardSpec()
	// Series filename: Details is the episode name, State is the tag.
	if d, s := def.Render("Show.S01E02.Pilot.1080p.mkv - PotPlayer",
		&Action{Running: true, HasStart: true, Start: 12345, CT: 1000}, Resolved{}); d != "Pilot" || s != "S1 E2" {
		t.Fatalf("series Render = (%q, %q), want (%q, %q)", d, s, "Pilot", "S1 E2")
	}
	// Movie with canonical override: Details is "Title (Year)", State joins
	// genres/length/synopsis with " . ".
	if d, s := def.Render("Dune.Part.Two.2024.1080p.mkv - PotPlayer",
		&Action{Running: true, HasStart: true, Start: 12345, CT: 1000},
		Resolved{Series: "Dune: Part Two", Year: 2024, IsMovie: true,
			Genres: "Sci-Fi", Length: "2h 46m", Synopsis: "Paul unites the Fremen"}); d != "Dune: Part Two (2024)" || s != "Sci-Fi \u00b7 2h 46m \u00b7 Paul unites the Fremen" {
		t.Fatalf("movie Render = (%q, %q), want goldens", d, s)
	}
	// Idle card keeps its shipped lines.
	if d, s := def.Render("", &Action{}, Resolved{}); d != "Browsing" || s != "Idle" {
		t.Fatalf("idle Render = (%q, %q), want (%q, %q)", d, s, "Browsing", "Idle")
	}
}

// TestCardSpecZeroFallsBack proves no migration is needed: the zero value
// renders exactly the shipped default on every input.
func TestCardSpecZeroFallsBack(t *testing.T) {
	var zero CardSpec
	def := DefaultCardSpec()
	if !zero.IsDefault() {
		t.Fatalf("zero CardSpec IsDefault = false, want true")
	}
	if got := zero.Normalized(); got != def {
		t.Fatalf("zero Normalized = %+v, want %+v", got, def)
	}
	for _, c := range cardSpecCases() {
		zd, zs := zero.Render(c.raw, c.act, c.res)
		dd, ds := def.Render(c.raw, c.act, c.res)
		if zd != dd || zs != ds {
			t.Errorf("%s: zero Render = (%q, %q), want default (%q, %q)",
				c.name, zd, zs, dd, ds)
		}
	}
}

// TestCardSpecUnknownFallsBack proves a stored typo can never reach the card.
func TestCardSpecUnknownFallsBack(t *testing.T) {
	weird := CardSpec{Details: "fancy-template", State: "\x00"}
	def := DefaultCardSpec()
	for _, c := range cardSpecCases() {
		gd, gs := weird.Render(c.raw, c.act, c.res)
		dd, ds := def.Render(c.raw, c.act, c.res)
		if gd != dd || gs != ds {
			t.Errorf("%s: unknown Render = (%q, %q), want default (%q, %q)",
				c.name, gd, gs, dd, ds)
		}
	}
}

// TestCardPresetsSmall proves the preset sets stay small, fixed, and
// default-first (presets, not a template language).
func TestCardPresetsSmall(t *testing.T) {
	d, s := CardPresets()
	if len(d) != 4 || len(s) != 4 {
		t.Fatalf("presets = %d details + %d states, want 4+4", len(d), len(s))
	}
	if d[0].ID != CardDetailsDefault || s[0].ID != CardStateDefault {
		t.Fatalf("first presets = %q/%q, want both %q", d[0].ID, s[0].ID, "default")
	}
	for _, p := range append(append([]CardPreset{}, d...), s...) {
		if p.ID == "" || p.Label == "" {
			t.Fatalf("preset %+v has an empty id or label", p)
		}
	}
}

// TestCardSpecPresetsCapped proves every preset line stays within the
// 128-unit Discord cap and the paused marker survives where expected.
func TestCardSpecPresetsCapped(t *testing.T) {
	dets, states := CardPresets()
	units := func(s string) int { return len(utf16.Encode([]rune(s))) }
	for _, c := range cardSpecCases() {
		for _, d := range dets {
			for _, st := range states {
				spec := CardSpec{Details: d.ID, State: st.ID}
				gd, gs := spec.Render(c.raw, c.act, c.res)
				if units(gd) > 128 {
					t.Errorf("%s %s/%s: details %d units > 128: %q",
						c.name, d.ID, st.ID, units(gd), gd)
				}
				if units(gs) > 128 {
					t.Errorf("%s %s/%s: state %d units > 128: %q",
						c.name, d.ID, st.ID, units(gs), gs)
				}
				if st.ID == CardStateNone && !isIdleTitle(c.raw) && gs != "" {
					t.Errorf("%s: Empty state = %q, want empty", c.name, gs)
				}
				if c.act != nil && c.act.Paused && !isIdleTitle(c.raw) &&
					st.ID != CardStateNone && gs != "Paused" && !strings.HasSuffix(gs, pausedMarker) {
					t.Errorf("%s %s/%s: paused state = %q, want suffix %q",
						c.name, d.ID, st.ID, gs, pausedMarker)
				}
			}
		}
	}
}

// TestCardSpecIdleStable proves the idle card keeps its shipped lines under
// any preset.
func TestCardSpecIdleStable(t *testing.T) {
	dets, states := CardPresets()
	for _, raw := range []string{"", "PotPlayer", "PotPlayer - PotPlayer"} {
		base := BuildActivityResolved(raw, &Action{}, Resolved{})
		for _, d := range dets {
			for _, st := range states {
				gd, gs := CardSpec{Details: d.ID, State: st.ID}.Render(raw, &Action{}, Resolved{})
				if gd != base.Details || gs != base.State {
					t.Errorf("idle %q %s/%s = (%q, %q), want (%q, %q)",
						raw, d.ID, st.ID, gd, gs, base.Details, base.State)
				}
			}
		}
	}
}

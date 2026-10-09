package metadata

import (
	"testing"
)

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Re:ZERO -Starting Life in Another World-", "re zero starting life in another world"},
		{"Mr. Robot", "mr robot"},
		{"  Breaking   Bad  ", "breaking bad"},
		{"Café Pokémon", "cafe pokemon"},
		{"Cat's in the Bag...", "cat s in the bag"},
		{"[Anime Time] Re Zero", "anime time re zero"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeTitle(c.in); got != c.want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTitleSimilarityExactAndRange(t *testing.T) {
	exact := [][2]string{
		{"Breaking Bad", "Breaking Bad"},
		{"Mr Robot", "Mr. Robot"},
		{"Black Bird", "Black Bird"},
		{"Naruto", "Naruto"},
	}
	for _, c := range exact {
		if got := titleSimilarity(c[0], c[1]); got != 1 {
			t.Errorf("titleSimilarity(%q,%q) = %v, want 1", c[0], c[1], got)
		}
	}
	for _, c := range [][2]string{
		{"Breaking Bad", "Re:ZERO -Starting Life in Another World-"},
		{"Re Zero", "Breaking Bad"},
		{"Black Bird", "Black Mirror"},
	} {
		got := titleSimilarity(c[0], c[1])
		if got < 0 || got > 1 {
			t.Errorf("titleSimilarity(%q,%q) = %v, want in [0,1]", c[0], c[1], got)
		}
		if got >= 0.5 {
			t.Errorf("titleSimilarity(%q,%q) = %v, want < 0.5 (unrelated)", c[0], c[1], got)
		}
	}
}

// TVmaze ranked the spinoff (weight 80) above the main show (weight 98);
// similarity must land them in one tie group so weight decides.
func TestPickBestShowReZero(t *testing.T) {
	cands := []ShowCand{
		{Name: "Re:Zero kara Hajimeru Break Time", Weight: 80},
		{Name: "Re:ZERO -Starting Life in Another World-", Weight: 98},
	}
	best, ok := pickBestShow("Re Zero", cands)
	if !ok {
		t.Fatal("pickBestShow(Re Zero) ok=false, want the main show")
	}
	if best.Name != "Re:ZERO -Starting Life in Another World-" {
		t.Fatalf("pickBestShow(Re Zero) = %q, want the main show", best.Name)
	}
	// Order-independent: spinoff listed second still loses.
	cands[0], cands[1] = cands[1], cands[0]
	best, ok = pickBestShow("Re Zero", cands)
	if !ok || best.Name != "Re:ZERO -Starting Life in Another World-" {
		t.Fatalf("pickBestShow(Re Zero, swapped) = %+v,%v; want the main show", best, ok)
	}
}

func TestPickBestShowConfident(t *testing.T) {
	cases := []struct {
		query string
		cands []ShowCand
		want  string
	}{
		{"Breaking Bad",
			[]ShowCand{{Name: "Breaking Bad", Weight: 99}, {Name: "Breaking Good", Weight: 10}},
			"Breaking Bad"},
		{"Mr Robot",
			[]ShowCand{{Name: "Mr. Robot", Weight: 90}, {Name: "Robot Wars", Weight: 50}},
			"Mr. Robot"},
		{"Black Bird",
			[]ShowCand{{Name: "Black Mirror", Weight: 95}, {Name: "Black Bird", Weight: 70}},
			"Black Bird"},
	}
	for _, c := range cases {
		best, ok := pickBestShow(c.query, c.cands)
		if !ok || best.Name != c.want {
			t.Errorf("pickBestShow(%q) = %+v,%v; want %q", c.query, best, ok, c.want)
		}
	}
}

func TestPickBestShowUnrelated(t *testing.T) {
	cands := []ShowCand{
		{Name: "Re:ZERO -Starting Life in Another World-", Weight: 98},
		{Name: "Naruto", Weight: 90},
	}
	if best, ok := pickBestShow("Breaking Bad", cands); ok {
		t.Fatalf("pickBestShow(Breaking Bad over anime) = %+v,true; want ok=false", best)
	}
	if _, ok := pickBestShow("Breaking Bad", nil); ok {
		t.Fatal("pickBestShow(nil) ok=true, want false")
	}
	if _, ok := pickBestShow("", []ShowCand{{Name: "Breaking Bad"}}); ok {
		t.Fatal("pickBestShow(empty query) ok=true, want false")
	}
}

// Equal-similarity ties break by Weight, then by EpisodeCount.
func TestPickBestShowTieBreaks(t *testing.T) {
	byWeight := []ShowCand{
		{Name: "Show Alpha", Weight: 10, EpisodeCount: 100},
		{Name: "Show Beta", Weight: 90, EpisodeCount: 1},
	}
	if best, ok := pickBestShow("Show", byWeight); !ok || best.Name != "Show Beta" {
		t.Fatalf("weight tie-break = %+v,%v; want Show Beta", best, ok)
	}
	byEps := []ShowCand{
		{Name: "Show Alpha", Weight: 10, EpisodeCount: 5},
		{Name: "Show Beta", Weight: 10, EpisodeCount: 40},
	}
	if best, ok := pickBestShow("Show", byEps); !ok || best.Name != "Show Beta" {
		t.Fatalf("episode-count tie-break = %+v,%v; want Show Beta", best, ok)
	}
}

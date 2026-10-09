package metadata

import "testing"

// Required vectors: the Mr.Harbor release tree, the Zentaro Season folder, a
// generics-only path, and the ResolveQuery merges on top. Note on
// punctuation: titleclean normalises dots to spaces, so the folder show for
// the Mr.Harbor tree is "Mr Harbor", not "Mr. Harbor" (the dotted canonical form
// arrives later from the provider resolution, proven live via metacheck).

const (
	mrHarborPath = `M:\1ARCHIVE\Mr.Harbor.COMPLETE.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Harbor.S01.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Harbor.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv`
	mrHarborFile = `Mr.Harbor.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv`
	zentaroPath  = `M:\2SERIES\Zentaro\Season 1\01.mkv`
)

func TestContextFromPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		show   string
		season int
	}{
		{"mr harbor release tree", mrHarborPath, "Mr Harbor", 1},
		{"zentaro season folder", zentaroPath, "Zentaro", 1},
		{"short season leaf", `M:\2FILMS\Show\S02\05.mkv`, "Show", 2},
		{"generics only", `M:\1ARCHIVE\Extras\Featurettes\Trailer.mkv`, "", 0},
		{"bare filename", "Trailer.mkv", "", 0},
		{"empty path", "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			show, season := ContextFromPath(tc.path)
			if show != tc.show || season != tc.season {
				t.Fatalf("ContextFromPath(%q) = (%q, %d), want (%q, %d)",
					tc.path, show, season, tc.show, tc.season)
			}
		})
	}
}

func TestResolveQuery(t *testing.T) {
	t.Run("bare number takes folder show", func(t *testing.T) {
		q := ResolveQuery("01.mkv", zentaroPath)
		want := Query{Kind: KindSeries, Title: "Zentaro", Season: 1, Episode: 1}
		if q != want {
			t.Fatalf("ResolveQuery(01.mkv, zentaro) = %+v, want %+v", q, want)
		}
	})
	t.Run("extra filename stays unknown", func(t *testing.T) {
		q := ResolveQuery("Deleted Scenes.mkv", `M:\1ARCHIVE\Show\Extras\Deleted Scenes.mkv`)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(Deleted Scenes.mkv, extras path) = %+v, want KindUnknown", q)
		}
	})
	t.Run("mr harbor filename and path", func(t *testing.T) {
		q := ResolveQuery(mrHarborFile, mrHarborPath)
		want := Query{Kind: KindSeries, Title: "Mr Harbor", Season: 1, Episode: 1}
		if q != want {
			t.Fatalf("ResolveQuery(mrharbor, mrharbor) = %+v, want %+v", q, want)
		}
	})
	t.Run("empty path is filename-only", func(t *testing.T) {
		q := ResolveQuery("01.mkv", "")
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(01.mkv, empty) = %+v, want KindUnknown", q)
		}
	})
	t.Run("theme file never rescues", func(t *testing.T) {
		q := ResolveQuery("[Studio Hour] Zentaro - OP - 001 - XA?B?C?D.mkv", zentaroPath)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(Zentaro OP, zentaro) = %+v, want KindUnknown", q)
		}
	})
	t.Run("bare number under extras stays unknown", func(t *testing.T) {
		// Synthetic inventory row: M:\2SERIES\Harbor Lights\Featurettes\Season 5\
		// Featurettes 5.1\Deleted Scenes\1.mkv (bucket F).
		q := ResolveQuery(`1.mkv`, `M:\2SERIES\Harbor Lights\Featurettes\Season 5\Featurettes 5.1\Deleted Scenes\1.mkv`)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(1.mkv, deleted-scenes path) = %+v, want KindUnknown", q)
		}
	})
	t.Run("short season leaf fills season", func(t *testing.T) {
		q := ResolveQuery("05.mkv", `M:\2FILMS\Show\S02\05.mkv`)
		want := Query{Kind: KindSeries, Title: "Show", Season: 2, Episode: 5}
		if q != want {
			t.Fatalf("ResolveQuery(05.mkv, S02 path) = %+v, want %+v", q, want)
		}
	})
	t.Run("bonus phrase folder stays unknown", func(t *testing.T) {
		// Folder-level mirror of the filename extraRe stems: a bare number
		// under a "Making Of" / "Gag Reel" leaf is bonus content, never
		// fetched, while the show above the leaf still resolves.
		for _, leaf := range []string{"Making Of", "Gag Reel", "Behind the Story", "TV Spots"} {
			path := `M:\1ARCHIVE\Show\` + leaf + `\01.mkv`
			if q := ResolveQuery("01.mkv", path); q.Kind != KindUnknown {
				t.Fatalf("ResolveQuery(01.mkv, %q) = %+v, want KindUnknown", path, q)
			}
			if !underExtrasFolder(path) {
				t.Fatalf("underExtrasFolder(%q) = false, want true", path)
			}
		}
		if show, _ := ContextFromPath(`M:\1ARCHIVE\Show\Gag Reel\clip.mkv`); show != "Show" {
			t.Fatalf("ContextFromPath(Gag Reel leaf) show = %q, want %q", show, "Show")
		}
	})
}

package metadata

import "testing"

// Required vectors: the Mr.Robot release tree, the Naruto Season folder, a
// generics-only path, and the ResolveQuery merges on top. Note on
// punctuation: titleclean normalises dots to spaces, so the folder show for
// the Mr.Robot tree is "Mr Robot", not "Mr. Robot" (the dotted canonical form
// arrives later from the provider resolution, proven live via metacheck).

const (
	mrRobotPath = `D:\Movies\Mr.Robot.COMPLETE.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Robot.S01.1080p.BluRay.AV1.DDP.5.1-dAV1nci\Mr.Robot.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv`
	mrRobotFile = `Mr.Robot.S01E01.1080p.BluRay.AV1.DDP.5.1-dAV1nci.mkv`
	narutoPath  = `D:\Shows\Naruto\Season 1\01.mkv`
)

func TestContextFromPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		show   string
		season int
	}{
		{"mr robot release tree", mrRobotPath, "Mr Robot", 1},
		{"naruto season folder", narutoPath, "Naruto", 1},
		{"short season leaf", `E:\Films\Show\S02\05.mkv`, "Show", 2},
		{"generics only", `D:\Movies\Extras\Featurettes\Trailer.mkv`, "", 0},
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
		q := ResolveQuery("01.mkv", narutoPath)
		want := Query{Kind: KindSeries, Title: "Naruto", Season: 1, Episode: 1}
		if q != want {
			t.Fatalf("ResolveQuery(01.mkv, naruto) = %+v, want %+v", q, want)
		}
	})
	t.Run("extra filename stays unknown", func(t *testing.T) {
		q := ResolveQuery("Deleted Scenes.mkv", `D:\Movies\Show\Extras\Deleted Scenes.mkv`)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(Deleted Scenes.mkv, extras path) = %+v, want KindUnknown", q)
		}
	})
	t.Run("mr robot filename and path", func(t *testing.T) {
		q := ResolveQuery(mrRobotFile, mrRobotPath)
		want := Query{Kind: KindSeries, Title: "Mr Robot", Season: 1, Episode: 1}
		if q != want {
			t.Fatalf("ResolveQuery(mrrobot, mrrobot) = %+v, want %+v", q, want)
		}
	})
	t.Run("empty path is filename-only", func(t *testing.T) {
		q := ResolveQuery("01.mkv", "")
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(01.mkv, empty) = %+v, want KindUnknown", q)
		}
	})
	t.Run("theme file never rescues", func(t *testing.T) {
		q := ResolveQuery("[Anime Time] Naruto - OP - 001 -  RO?C?K?S.mkv", narutoPath)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(Naruto OP, naruto) = %+v, want KindUnknown", q)
		}
	})
	t.Run("bare number under extras stays unknown", func(t *testing.T) {
		// Real inventory row: D:\Shows\Breaking Bad\Featurettes\Season 5\
		// Featurettes 5.1\Deleted Scenes\1.mkv (bucket F).
		q := ResolveQuery(`1.mkv`, `D:\Shows\Breaking Bad\Featurettes\Season 5\Featurettes 5.1\Deleted Scenes\1.mkv`)
		if q.Kind != KindUnknown {
			t.Fatalf("ResolveQuery(1.mkv, deleted-scenes path) = %+v, want KindUnknown", q)
		}
	})
	t.Run("short season leaf fills season", func(t *testing.T) {
		q := ResolveQuery("05.mkv", `E:\Films\Show\S02\05.mkv`)
		want := Query{Kind: KindSeries, Title: "Show", Season: 2, Episode: 5}
		if q != want {
			t.Fatalf("ResolveQuery(05.mkv, S02 path) = %+v, want %+v", q, want)
		}
	})
}

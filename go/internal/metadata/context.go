package metadata

import (
	"regexp"
	"strings"

	"potflix/internal/titleclean"
)

// context.go - folder context for filename-poor media.
//
// Many anime files carry no usable show in the filename; the show lives in
// the folder (e.g. ...\Naruto\Season 1\01.mkv, or a release-style tree like
// ...\Mr.Robot.S01.1080p... \Mr.Robot.S01E01... .mkv). ContextFromPath reads
// the show/season out of the FULL playing-file path (player.CurrentFile) and
// ResolveQuery merges that context into the filename-only ParseRaw result.

var (
	// seasonDirRe / seasonShortRe match a whole folder that IS a season
	// ("Season 1", "S01"). Applied per path component, root first.
	seasonDirRe   = regexp.MustCompile(`(?i)^Season\s*(\d+)$`)
	seasonShortRe = regexp.MustCompile(`(?i)^S(\d{1,2})$`)
	// seasonEmbeddedRe finds a season token inside a release-style folder
	// ("Mr.Robot.S01.1080p...", "Show.Season.2.BluRay"). Fallback only: the
	// exact pass above wins, and the deepest component wins.
	seasonEmbeddedRe = regexp.MustCompile(`(?i)(?:^|[.\s_\-])(?:Season\s*(\d{1,2})|S(\d{1,2}))(?:$|[.\s_\-])`)
	// genericFolderRe matches whole folders that are never a show: season
	// tokens, disc/part/cour leaves ("01. Series"), quality/source tags and
	// creditless-theme leaves (NC/NCOP/NCED/OP/ED).
	genericFolderRe = regexp.MustCompile(`(?i)^(featurettes?|extras?|specials?|subs?|subtitles?|bonus|season\s*\d+|series\s*\d+|s\d{1,2}|bd|bdrip|brrip|1080p|720p|480p|2160p|4k|nc|ncop|nced|op|ed|disc\s*\d*|disk\s*\d*|part\s*\d+|cour\s*\d+|vol\s*\d*|\d{1,2}\s*[.\-]\s*(series|season|disc|disk|part|cour))$`)
	// extrasWordRe spots an extras folder anywhere in the component
	// ("Deleted Scenes", "Featurettes 5.1", "Trailer", "NCED"). Substring on
	// word boundaries, mirroring the filename-level extraRe; bare OP/ED are
	// deliberately absent here ("Speed" must never match - full-match only
	// in genericFolderRe above).
	extrasWordRe = regexp.MustCompile(`(?i)\b(deleted\s+scenes?|trailers?|gallery|alternate\s+endings?|interviews?|behind\s+the\s+scenes|featurettes?|music\s+videos?|samples?|extras?|specials?|ncop|nced|\bnc\b)\b`)
	// qualityTitleRe rejects cleaned folder titles that are bare
	// numbers/quality tokens, mirroring the Parse floor.
	qualityTitleRe = regexp.MustCompile(`(?i)^(\d+|1080p|720p|480p|360p|2160p|4k|uhd|bd|bdrip|brrip|10bit|8bit)$`)
	// driveRe spots a Windows volume component ("C:").
	driveRe = regexp.MustCompile(`^[A-Za-z]:$`)
	// rootLookRe spots a library-root-looking top folder: no lowercase
	// letters (MOVIES, SHOWS, Anime). Show folders carry
	// lowercase (Naruto, Mr.Robot.S01...).
	rootLookRe = regexp.MustCompile(`^[^a-z]*$`)
	// seasonSuffixRe trims a trailing season token off a cleaned folder show
	// ("Mr Robot S01" -> "Mr Robot").
	seasonSuffixRe = regexp.MustCompile(`(?i)\s+S\d{1,2}$`)
	// videoExtRe strips one trailing media extension before number hunting.
	videoExtRe = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|m4v|mov|wmv|ts|flv|webm|mpg|mpeg|m4v|ass|srt)$`)
	// epNumRe finds separator-delimited numbers. Dots are NOT delimiters, so
	// version fragments (5.1, 3.0), quality tokens (1080p, 10bit, x265) and
	// joined tags (S01E01, E2015) never match; years are rejected by the
	// caller via isYearNum.
	epNumRe = regexp.MustCompile(`(?:^|[\s_\-\[\]();]+)(\d{1,4})(?:$|[\s_\-\[\]();]+)`)
)

// splitDirs splits a full path into components on either separator, drops the
// trailing filename, and reports the index of the top folder under the volume
// (the library root: D:\Movies, E:\Anime, ...). Both
// separators are accepted so unit paths and live paths behave alike.
func splitDirs(path string) (dirs []string, top int) {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return nil, 0
	}
	dirs = parts[:len(parts)-1]
	top = 0
	for top < len(dirs) && (driveRe.MatchString(dirs[top]) || dirs[top] == ".") {
		top++
	}
	return dirs, top
}

// isGenericFolder reports whether a path component can never be a show name.
func isGenericFolder(name string) bool {
	if seasonDirRe.MatchString(name) || seasonShortRe.MatchString(name) {
		return true
	}
	if genericFolderRe.MatchString(name) || extrasWordRe.MatchString(name) {
		return true
	}
	if t := titleclean.Clean(name).Title; t != "" {
		if genericFolderRe.MatchString(t) || extrasWordRe.MatchString(t) {
			return true
		}
	}
	return false
}

// underExtrasFolder reports whether any folder on the path is an extras leaf
// (Deleted Scenes, Featurettes, ...). Such files stay KindUnknown even when a
// bare number is present: the inventory buckets them F.
func underExtrasFolder(path string) bool {
	dirs, _ := splitDirs(path)
	for _, d := range dirs {
		if extrasWordRe.MatchString(d) {
			return true
		}
		if t := titleclean.Clean(d).Title; t != "" && extrasWordRe.MatchString(t) {
			return true
		}
	}
	return false
}

// ContextFromPath derives (show, season) from a full playing-file path. The
// filename is dropped. Season is the root-first exact "Season N"/"SNN" folder,
// falling back to the deepest embedded release-style token (".S01."). Show is
// the deepest surviving folder: drives, the library-root top folder when it
// looks like a root (no lowercase: MOVIES, SHOWS, ...), season and generic
// folders are skipped, and the survivor is titleclean-normalised (dots become
// spaces). ("", 0) when nothing usable remains.
func ContextFromPath(path string) (string, int) {
	if strings.TrimSpace(path) == "" {
		return "", 0
	}
	dirs, top := splitDirs(path)
	if len(dirs) == 0 {
		return "", 0
	}
	season := 0
	for _, d := range dirs {
		if m := seasonDirRe.FindStringSubmatch(d); m != nil {
			season = atoi(m[1])
			break
		}
		if m := seasonShortRe.FindStringSubmatch(d); m != nil {
			season = atoi(m[1])
			break
		}
	}
	if season == 0 {
		for i := len(dirs) - 1; i >= 0; i-- {
			if m := seasonEmbeddedRe.FindStringSubmatch(dirs[i]); m != nil {
				s := m[1]
				if s == "" {
					s = m[2]
				}
				if n := atoi(s); n > 0 {
					season = n
					break
				}
			}
		}
	}
	show := ""
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if d == "" || d == "." || d == ".." || driveRe.MatchString(d) {
			continue
		}
		// The volume-top folder is the library root (the inventory roots are
		// all volume tops; root-direct files carry no show). It is skipped
		// only when it looks like a root rather than a show: library roots
		// carry no lowercase letters (MOVIES, SHOWS, Anime) while show folders do (Naruto, Mr.Robot.S01...).
		if i == top && rootLookRe.MatchString(d) {
			continue
		}
		if isGenericFolder(d) {
			continue
		}
		t := strings.TrimSpace(seasonSuffixRe.ReplaceAllString(strings.TrimSpace(titleclean.Clean(d).Title), ""))
		if t == "" || qualityTitleRe.MatchString(t) || !lettersRe.MatchString(t) {
			continue
		}
		show = t
		break
	}
	if show == "" {
		return "", 0
	}
	return show, season
}

// bareEpisodeNumber reads one episode number from a raw filename: the first
// separator-delimited run that is not a year (1900-2100). Dots never delimit,
// so versions (5.1), quality (1080p/10bit/x265) and joined tags (S01E01)
// cannot match. 0 when none.
func bareEpisodeNumber(rawTitle string) int {
	s := strings.TrimSuffix(rawTitle, potPlayerSuffix)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if m := videoExtRe.FindStringIndex(s); m != nil {
		s = s[:m[0]]
	}
	for _, m := range epNumRe.FindAllStringSubmatch(s, -1) {
		if n := atoi(m[1]); n > 0 && !isYearNum(n) {
			return n
		}
	}
	return 0
}

// ResolveQuery merges folder context into the filename-only ParseRaw result.
// An empty path falls back cleanly to filename-only.
//
//   - KindUnknown: a bare-number filename ("01.mkv") plus a usable folder show
//     becomes a series query (absolute when the folders carry no season).
//     Themes, filename-level extras, and files under extras folders stay
//     Unknown, so they are never fetched or cached.
//   - KindSeries: an empty title takes the folder show; season fills from the
//     folders only for non-absolute queries; a still-empty title demotes to
//     Unknown.
//   - KindMovie: an empty title takes the folder show.
func ResolveQuery(rawTitle, path string) Query {
	q := ParseRaw(rawTitle)
	if strings.TrimSpace(path) == "" {
		return q
	}
	show, season := ContextFromPath(path)
	switch q.Kind {
	case KindUnknown:
		if show == "" || underExtrasFolder(path) {
			return q
		}
		c := titleclean.Clean(strings.TrimSuffix(rawTitle, potPlayerSuffix))
		if isTheme(c.Title, c.EpisodeTag) || extraRe.MatchString(c.Title) {
			return q
		}
		if n := bareEpisodeNumber(rawTitle); n > 0 {
			return Query{Kind: KindSeries, Title: show, Season: season, Episode: n, Absolute: season == 0}
		}
		return q
	case KindSeries:
		if q.Title == "" {
			q.Title = show
		}
		if q.Season == 0 && season > 0 && !q.Absolute {
			q.Season = season
		}
		if q.Title == "" {
			q.Kind = KindUnknown
		}
		return q
	default: // KindMovie
		if q.Title == "" && show != "" {
			q.Title = show
		}
		return q
	}
}

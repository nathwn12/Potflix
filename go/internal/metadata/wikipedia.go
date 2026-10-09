package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// wikipediaSource is the keyless movie fallback behind iTunes: movies
// iTunes misses still get a title, year, poster and synopsis from the
// keyless Wikipedia Action API (no key, no account, free anonymous use).
//
// Two calls on the base path: list=search picks the top film hit, then
// prop=extracts|pageimages yields the synopsis (extract) and, when one
// exists, the poster (thumbnail). MEASURED 2026-10-10 against the live
// API: PageImages never returns film posters (they are fair-use, and
// PageImages serves freely-licensed images only - verified on three
// real articles, thumbnail absent on all), so a page that resolves with
// an empty thumbnail gets at most TWO best-effort same-host calls through
// the SAME Action API: prop=images lists the article's files, and the
// poster-like file (name contains "poster", case-insensitive, ending
// .jpg/.jpeg preferred, then .png, then .webp; anything else rejected)
// is resolved to a direct URL via prop=imageinfo&iiprop=url. When no
// poster-like file exists Artwork stays empty - never invented, never a
// substitute (cast photo, logo, screenshot: a wrong poster is worse than
// none). What Wikipedia reliably yields - and what the card was missing -
// is title, year and synopsis, plus the article URL as the View-Movie
// button. The year comes from the page title or extract text; when
// neither yields one it stays empty - never invented.
// Genres/Length stay empty (no keyless field carries them).
type wikipediaSource struct {
	base   string
	client *http.Client
}

func (s *wikipediaSource) Name() string { return "wikipedia" }

func (s *wikipediaSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://en.wikipedia.org"
}

func (s *wikipediaSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

// wikipediaUserAgent identifies the app per the Wikimedia User-Agent
// policy (descriptive app identifier, never a credential): every
// Wikipedia fetch below sends this instead of the shared metadata UA.
const wikipediaUserAgent = "Potflix/1.0 (https://github.com/nathwn12/Potflix; contact: potflix-app)"

// wikiGet performs a GET with the Wikimedia descriptive User-Agent and one
// bounded 429 retry. The URL is never logged; errors carry the status only.
func (s *wikipediaSource) wikiGet(ctx context.Context, fullURL string) (*http.Response, error) {
	return fetchWithRetry(ctx, s.httpClient(), func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", wikipediaUserAgent)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
}

// wikiTransient wraps a transient failure with the sentinel so the
// resolver's honest counters stay honest: a wrapped error is transient
// (persist nothing), while (nil, nil) is a definitive miss. The cause is
// sanitized: the request URL (which carries the escaped title) and any
// URL-like payload never reach the message, so an owner title cannot land
// in a log via the resolver's error log.
func wikiTransient(cause error) error {
	return fmt.Errorf("metadata: wikipedia lookup unavailable (%s): %w", wikiSanitizeCause(cause), ErrLookupUnavailable)
}

// wikiSanitizeCause reports a title-free diagnostic for cause. Transport
// (*url.Error) messages embed the request URL - which carries the escaped
// search title - so only the operation is kept and the URL is redacted.
// Any other message carrying an http(s) URL is redacted the same way;
// URL-free diagnostics (status, decode, context) pass through verbatim.
func wikiSanitizeCause(cause error) string {
	var uerr *url.Error
	if errors.As(cause, &uerr) {
		op := strings.TrimSpace(uerr.Op)
		if op == "" {
			op = "request"
		}
		return op + " failed (URL redacted)"
	}
	s := strings.TrimSpace(cause.Error())
	if s == "" {
		return "request failed (URL redacted)"
	}
	if strings.Contains(s, "http://") || strings.Contains(s, "https://") || strings.Contains(s, "api.php") {
		return "request failed (URL redacted)"
	}
	return s
}

// wikiAPIError is the Action API error envelope served with HTTP 200
// (e.g. {"error":{"code":"maxlag","info":"Waiting..."}}): Query is empty,
// so without this check the empty result reads as a definitive miss and
// the resolver would persist a negative for a lookup that never happened.
type wikiAPIError struct {
	Code string `json:"code"`
	Info string `json:"info"`
}

// wikiMissingBool accepts the API's missing marker in either shape: the
// live API serves {"missing":""} (string), while some fixtures use
// {"missing":true} (bool). Presence of the key always means missing;
// absent means present. Any string value marks missing; bools follow
// their value; null/absent means present.
type wikiMissingBool bool

// UnmarshalJSON implements json.Unmarshaler for wikiMissingBool.
func (b *wikiMissingBool) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		*b = false
		return nil
	}
	switch data[0] {
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*b = true
		return nil
	case 't', 'f':
		var v bool
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*b = wikiMissingBool(v)
		return nil
	case 'n':
		*b = false
		return nil
	default:
		return fmt.Errorf("metadata: invalid missing value %s", string(data))
	}
}

// wikiSearchResp is GET w/api.php?action=query&list=search.
type wikiSearchResp struct {
	Error *wikiAPIError `json:"error"`
	Query struct {
		Search []struct {
			Title string `json:"title"`
		} `json:"search"`
	} `json:"query"`
}

// wikiDetailResp is GET w/api.php?action=query&prop=extracts|pageimages.
// Pages is keyed by pageid; a "missing" page carries {"missing":""}.
type wikiDetailResp struct {
	Error *wikiAPIError `json:"error"`
	Query struct {
		Pages map[string]struct {
			Title     string          `json:"title"`
			Missing   wikiMissingBool `json:"missing"`
			Extract   string          `json:"extract"`
			Thumbnail struct {
				Source string `json:"source"`
			} `json:"thumbnail"`
		} `json:"pages"`
	} `json:"query"`
}

// wikiImagesResp is GET w/api.php?action=query&prop=images: the file
// names used on one article. Pages is keyed by pageid.
type wikiImagesResp struct {
	Query struct {
		Pages map[string]struct {
			Images []struct {
				Title string `json:"title"`
			} `json:"images"`
		} `json:"pages"`
	} `json:"query"`
}

// wikiImageInfoResp is GET
// w/api.php?action=query&prop=imageinfo&iiprop=url&titles=File:<name>:
// the direct file URL. Pages is keyed by pageid.
type wikiImageInfoResp struct {
	Query struct {
		Pages map[string]struct {
			Missing   wikiMissingBool `json:"missing"`
			ImageInfo []struct {
				URL string `json:"url"`
			} `json:"imageinfo"`
		} `json:"pages"`
	} `json:"query"`
}

// wikiPosterExtRank ranks a plausible poster photo extension: .jpg/.jpeg
// first, then .png, then .webp. Anything else (.svg, .gif, extensionless,
// ...) is implausible for a film poster and rejected (ok=false).
func wikiPosterExtRank(lower string) (rank int, ok bool) {
	switch {
	case strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg"):
		return 0, true
	case strings.HasSuffix(lower, ".png"):
		return 1, true
	case strings.HasSuffix(lower, ".webp"):
		return 2, true
	}
	return 0, false
}

// wikiPosterBlocked reports whether a lowercased file name looks like
// documentation about posters rather than a poster itself (guidelines,
// logos, lists, sheets, styles). Such names contain "poster" but are
// never the artwork - picking one would invent a wrong poster.
func wikiPosterBlocked(lower string) bool {
	for _, bad := range []string{"guideline", "logo", "list", "sheet", "style"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

// wikiPosterPlausible reports whether "poster" appears as a standalone
// word (plural "posters" allowed), not as part of a longer word such as
// "posterize". The check scans every "poster" occurrence for non-letter
// boundaries; one plausible occurrence suffices.
func wikiPosterPlausible(lower string) bool {
	for i := 0; i+6 <= len(lower); {
		idx := strings.Index(lower[i:], "poster")
		if idx < 0 {
			return false
		}
		start := i + idx
		beforeOK := start == 0 || !isASCIILetter(lower[start-1])
		after := start + 6
		afterOK := false
		switch {
		case after >= len(lower):
			afterOK = true
		case !isASCIILetter(lower[after]):
			afterOK = true
		case lower[after] == 's' || lower[after] == 'S':
			// Allow plural "posters": trailing s must itself be bounded.
			if after+1 >= len(lower) || !isASCIILetter(lower[after+1]) {
				afterOK = true
			}
		}
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
	return false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// wikiPickPoster returns the poster-like File: title from an article image
// list, or "". The rule is deliberately strict - a wrong poster is worse
// than none: the file name must contain "poster" as a standalone word
// (case-insensitive, plural allowed) AND end in a plausible photo
// extension (.jpg/.jpeg preferred, then .png, then .webp), and must not
// look like documentation about posters (guideline/logo/list/sheet/style
// in the name). When several match, the best extension wins, ties broken
// by first listed. Anything else - a cast photo, a logo, a screenshot, a
// "poster guidelines" doc - is rejected, so an ambiguous list yields "".
func wikiPickPoster(titles []string) string {
	best := ""
	bestRank := 0
	found := false
	for _, t := range titles {
		name := strings.TrimSpace(t)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !strings.Contains(lower, "poster") {
			continue
		}
		if wikiPosterBlocked(lower) {
			continue
		}
		if !wikiPosterPlausible(lower) {
			continue
		}
		rank, ok := wikiPosterExtRank(lower)
		if !ok {
			continue
		}
		if !found || rank < bestRank {
			best, bestRank, found = name, rank, true
		}
	}
	return best
}

// wikiPosterURL is best-effort (never transient, never a miss): at most
// two SAME-host calls - prop=images lists the article's files, then
// prop=imageinfo&iiprop=url resolves the picked poster file to a direct
// URL. Any failure, any non-poster list, or any empty URL yields "" and
// the caller keeps Artwork empty (today's behavior). It never invents or
// substitutes an image, and it runs ONLY for an already-resolved page.
func (s *wikipediaSource) wikiPosterURL(ctx context.Context, page string) string {
	if strings.TrimSpace(page) == "" {
		return ""
	}
	if err := ctx.Err(); err != nil {
		return ""
	}
	listURL := s.baseURL() + "/w/api.php?action=query&prop=images&titles=" +
		url.QueryEscape(page) + "&format=json"
	resp, err := s.wikiGet(ctx, listURL)
	if err != nil {
		return ""
	}
	var lr wikiImagesResp
	if err := decodeCapped(resp, &lr); err != nil {
		return ""
	}
	var names []string
	for _, p := range lr.Query.Pages {
		for _, im := range p.Images {
			names = append(names, im.Title)
		}
	}
	picked := wikiPickPoster(names)
	if picked == "" {
		return ""
	}
	if err := ctx.Err(); err != nil {
		return ""
	}
	infoURL := s.baseURL() + "/w/api.php?action=query&prop=imageinfo&iiprop=url&titles=" +
		url.QueryEscape(picked) + "&format=json"
	resp, err = s.wikiGet(ctx, infoURL)
	if err != nil {
		return ""
	}
	var ir wikiImageInfoResp
	if err := decodeCapped(resp, &ir); err != nil {
		return ""
	}
	for _, p := range ir.Query.Pages {
		if p.Missing {
			continue
		}
		for _, ii := range p.ImageInfo {
			if u := strings.TrimSpace(ii.URL); u != "" {
				return u
			}
		}
	}
	return ""
}

// wikiParenYear matches "(2010)" or "(2010 film)" in a page title.
var wikiParenYear = regexp.MustCompile(`\((19\d{2}|20\d{2})\b`)

// wikiAnyYear finds a plausible release year anywhere in text.
var wikiAnyYear = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)

// wikiYear extracts the release year from the page title first ("Dune
// (2021 film)" -> 2021), then from the extract text, and returns 0 when
// neither yields one. It never invents a year.
func wikiYear(pageTitle, extract string) int {
	if m := wikiParenYear.FindStringSubmatch(pageTitle); m != nil {
		if y, err := strconv.Atoi(m[1]); err == nil && y >= 1900 && y <= 2100 {
			return y
		}
	}
	if m := wikiAnyYear.FindString(extract); m != "" {
		if y, err := strconv.Atoi(m); err == nil && y >= 1900 && y <= 2100 {
			return y
		}
	}
	return 0
}

// wikiPageURL is the canonical article URL for a page title.
func (s *wikipediaSource) wikiPageURL(pageTitle string) string {
	return normalizeURL(s.baseURL() + "/wiki/" + url.PathEscape(strings.ReplaceAll(pageTitle, " ", "_")))
}

// Find is movie-only (a series key never reaches the wire): it returns
// (nil, nil) for series or blank titles BEFORE any fetch, a wrapped
// ErrLookupUnavailable on transient failure, and (nil, nil) on a
// definitive "no such page".
func (s *wikipediaSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Kind == KindSeries || q.Title == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	searchURL := s.baseURL() + "/w/api.php?action=query&list=search&srsearch=" +
		url.QueryEscape(q.Title+" film") + "&srlimit=5&format=json"
	resp, err := s.wikiGet(ctx, searchURL)
	if err != nil {
		return nil, wikiTransient(err)
	}
	var sr wikiSearchResp
	if err := decodeCapped(resp, &sr); err != nil {
		return nil, wikiTransient(err)
	}
	if sr.Error != nil {
		code := strings.TrimSpace(sr.Error.Code)
		if code == "" {
			code = "unknown"
		}
		return nil, wikiTransient(errors.New("wikipedia api error " + code))
	}
	page := ""
	for _, h := range sr.Query.Search {
		if t := strings.TrimSpace(h.Title); t != "" {
			page = t
			break
		}
	}
	if page == "" {
		return nil, nil
	}

	detailURL := s.baseURL() + "/w/api.php?action=query&prop=extracts|pageimages&exintro&explaintext" +
		"&piprop=thumbnail&pithumbsize=600&titles=" + url.QueryEscape(page) + "&format=json"
	resp, err = s.wikiGet(ctx, detailURL)
	if err != nil {
		return nil, wikiTransient(err)
	}
	var dr wikiDetailResp
	if err := decodeCapped(resp, &dr); err != nil {
		return nil, wikiTransient(err)
	}
	if dr.Error != nil {
		code := strings.TrimSpace(dr.Error.Code)
		if code == "" {
			code = "unknown"
		}
		return nil, wikiTransient(errors.New("wikipedia api error " + code))
	}
	if len(dr.Query.Pages) == 0 {
		return nil, nil
	}
	var title, extract, thumb string
	matched := false
	for _, p := range dr.Query.Pages {
		if p.Missing {
			continue
		}
		if strings.TrimSpace(p.Title) == "" && strings.TrimSpace(p.Extract) == "" && strings.TrimSpace(p.Thumbnail.Source) == "" {
			continue
		}
		title, extract, thumb = p.Title, p.Extract, p.Thumbnail.Source
		matched = true
		break
	}
	if !matched {
		return nil, nil
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = strings.TrimSpace(q.Title)
	}
	// Poster fallback, same host, at most two extra calls, ONLY for an
	// already-resolved page whose thumbnail came back empty: a present
	// thumbnail returns untouched above (no extra calls on that path).
	artwork := strings.TrimSpace(thumb)
	if artwork == "" {
		artwork = s.wikiPosterURL(ctx, page)
	}
	return &Media{
		Source: "wikipedia", SourceID: title,
		Kind: KindMovie, Title: title, Year: wikiYear(title, extract),
		Artwork:   normalizeURL(artwork),
		Synopsis:  capSynopsis(stripHTML(extract)),
		MovieURL:  s.wikiPageURL(title),
		FetchedAt: time.Now().UTC(),
	}, nil
}

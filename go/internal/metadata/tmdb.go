package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tmdbSource is the keyed TMDB provider. It answers only when constructed
// with a non-blank key (the resolver omits it otherwise) and implements the
// same Source contract as tvmazeSource: a miss is (nil, nil) so the resolver
// falls through to the keyless chain, while transport/HTTP failures are
// errors (which the resolver also skips past - a bad key never breaks a
// lookup).
//
// Matching discipline mirrors tvmaze: search results are re-ranked by
// pickBestShow (title similarity with weight tie-breaks, low confidence is
// a miss), a known query year rejects differently-dated candidates, and a
// requested season that does not exist is a miss - never a positional guess.
//
// Secrecy: the api_key query parameter must never reach the debug log, an
// error string, or any %v of a request. Every error leaving this source is
// passed through redact, which strips the key (raw and escaped) plus any
// api_key=... parameter remnant.
type tmdbSource struct {
	base   string
	key    string
	client *http.Client
}

func (s *tmdbSource) Name() string { return "tmdb" }

func (s *tmdbSource) baseURL() string {
	if s.base != "" {
		return s.base
	}
	return "https://api.themoviedb.org"
}

func (s *tmdbSource) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return newHTTPClient(0)
}

// tmdbKeyParamRe strips any api_key=... parameter remnant from an error
// string, whatever its value form.
var tmdbKeyParamRe = regexp.MustCompile(`api_key=[^&\s"']*`)

// redact strips the key from err so the resolver's "source tmdb error: %v"
// log line can never carry it.
func (s *tmdbSource) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if s.key != "" {
		msg = strings.ReplaceAll(msg, s.key, "[redacted]")
		if esc := url.QueryEscape(s.key); esc != s.key {
			msg = strings.ReplaceAll(msg, esc, "[redacted]")
		}
	}
	msg = tmdbKeyParamRe.ReplaceAllString(msg, "api_key=[redacted]")
	return fmt.Errorf("metadata: tmdb request failed: %s", msg)
}

// tmdbGet performs one keyed GET against path. params are extra query
// values (api_key is always attached first).
func (s *tmdbSource) tmdbGet(ctx context.Context, path string, params map[string]string) (*http.Response, error) {
	u := s.baseURL() + path + "?api_key=" + url.QueryEscape(s.key)
	for k, v := range params {
		u += "&" + k + "=" + url.QueryEscape(v)
	}
	resp, err := fetchGet(ctx, s.httpClient(), u)
	if err != nil {
		return nil, s.redact(err)
	}
	return resp, nil
}

// tmdbDecode decodes a 2xx JSON body; a 404 is a miss (nil, nil) so the
// resolver falls through, while any other non-2xx status is an error (which
// the resolver likewise skips past). The body is never logged.
func tmdbDecode(resp *http.Response, v any, redact func(error) error) error {
	if resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp)
		return errTMDBMiss
	}
	if err := decodeCapped(resp, v); err != nil {
		return redact(err)
	}
	return nil
}

// errTMDBMiss is the sentinel for "matched nothing": Find converts it to
// (nil, nil).
var errTMDBMiss = fmt.Errorf("metadata: tmdb miss")

func tmdbMiss(err error) bool { return err == errTMDBMiss }

// tmdbArt renders a TMDB poster_path as artwork, "" when absent.
func tmdbArt(posterPath string) string {
	if strings.TrimSpace(posterPath) == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w500" + strings.TrimSpace(posterPath)
}

type tmdbTVHit struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	FirstAir  string `json:"first_air_date"`
	VoteCount int    `json:"vote_count"`
}

type tmdbTVSearchResp struct {
	Results []tmdbTVHit `json:"results"`
}

type tmdbGenre struct {
	Name string `json:"name"`
}

type tmdbTVSeasonRef struct {
	SeasonNumber int `json:"season_number"`
}

type tmdbTVDetail struct {
	ID         int               `json:"id"`
	Name       string            `json:"name"`
	FirstAir   string            `json:"first_air_date"`
	PosterPath string            `json:"poster_path"`
	Overview   string            `json:"overview"`
	Genres     []tmdbGenre       `json:"genres"`
	RunTime    []int             `json:"episode_run_time"`
	Seasons    []tmdbTVSeasonRef `json:"seasons"`
}

type tmdbSeasonEp struct {
	EpisodeNumber int    `json:"episode_number"`
	SeasonNumber  int    `json:"season_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	// still_path is deliberately ignored: artwork is ALWAYS the series
	// poster (cover rework - stills look awkward/unofficial).
}

type tmdbSeasonResp struct {
	SeasonNumber int            `json:"season_number"`
	Episodes     []tmdbSeasonEp `json:"episodes"`
}

type tmdbMovieHit struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Release   string `json:"release_date"`
	VoteCount int    `json:"vote_count"`
}

type tmdbMovieSearchResp struct {
	Results []tmdbMovieHit `json:"results"`
}

type tmdbMovieDetail struct {
	ID         int         `json:"id"`
	Title      string      `json:"title"`
	Release    string      `json:"release_date"`
	PosterPath string      `json:"poster_path"`
	Overview   string      `json:"overview"`
	Genres     []tmdbGenre `json:"genres"`
	Runtime    int         `json:"runtime"`
}

// maxTMDBSeasons bounds the season fan-out for absolute/Season==0 series
// queries (one request per season).
const maxTMDBSeasons = 30

// Find resolves one series season (Season>0, !Absolute) via the season
// endpoint, the whole show in air order otherwise, or one movie via its
// detail endpoint.
func (s *tmdbSource) Find(ctx context.Context, q Query) (*Media, error) {
	if q.Title == "" || strings.TrimSpace(s.key) == "" {
		return nil, nil
	}
	switch q.Kind {
	case KindMovie:
		return s.findMovie(ctx, q)
	case KindSeries:
		return s.findSeries(ctx, q)
	default:
		return nil, nil
	}
}

// tmdbGenreNames joins TMDB detail genre names with the shared separator.
func tmdbGenreNames(gs []tmdbGenre) string {
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		if t := strings.TrimSpace(g.Name); t != "" {
			names = append(names, t)
		}
	}
	return joinGenres(names)
}

func tmdbSeriesLength(run []int) string {
	for _, m := range run {
		if m > 0 {
			return runtimeLength(m)
		}
	}
	return ""
}

func (s *tmdbSource) findSeries(ctx context.Context, q Query) (*Media, error) {
	params := map[string]string{"query": q.Title}
	if q.Year > 0 {
		params["first_air_date_year"] = strconv.Itoa(q.Year)
	}
	resp, err := s.tmdbGet(ctx, "/3/search/tv", params)
	if err != nil {
		return nil, err
	}
	var sr tmdbTVSearchResp
	if err := tmdbDecode(resp, &sr, s.redact); err != nil {
		if tmdbMiss(err) {
			return nil, nil
		}
		return nil, err
	}
	// Carry vote counts into the ranking as weights (more-voted entries
	// break near-ties, mirroring TVmaze-weight tie-breaks).
	cands := make([]ShowCand, 0, len(sr.Results))
	kept := make([]tmdbTVHit, 0, len(sr.Results))
	for _, h := range sr.Results {
		if h.ID == 0 || strings.TrimSpace(h.Name) == "" {
			continue
		}
		if q.Year > 0 {
			if y := dateYear(h.FirstAir); y > 0 && y != q.Year {
				continue
			}
		}
		cands = append(cands, ShowCand{Name: h.Name, Weight: h.VoteCount})
		kept = append(kept, h)
	}
	winner, ok := pickBestShow(q.Title, cands)
	if !ok {
		return nil, nil
	}
	var hit tmdbTVHit
	found := false
	for i, c := range cands {
		if c == winner {
			hit = kept[i]
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}

	resp, err = s.tmdbGet(ctx, "/3/tv/"+strconv.Itoa(hit.ID), nil)
	if err != nil {
		return nil, err
	}
	var det tmdbTVDetail
	if err := tmdbDecode(resp, &det, s.redact); err != nil {
		if tmdbMiss(err) {
			return nil, nil
		}
		return nil, err
	}
	year := dateYear(det.FirstAir)
	genres := tmdbGenreNames(det.Genres)
	length := tmdbSeriesLength(det.RunTime)
	showURL := normalizeURL("https://www.themoviedb.org/tv/" + strconv.Itoa(hit.ID))

	if q.Season > 0 && !q.Absolute {
		eps, err := s.tmdbSeason(ctx, hit.ID, q.Season)
		if err != nil {
			if tmdbMiss(err) {
				return nil, nil
			}
			return nil, err
		}
		return &Media{
			Source: "tmdb", SourceID: strconv.Itoa(hit.ID),
			Kind: KindSeries, Title: firstNonEmpty(det.Name, hit.Name), Year: year,
			Artwork: tmdbArt(det.PosterPath), Synopsis: capSynopsis(stripHTML(det.Overview)),
			Genres: genres, Length: length, ShowURL: showURL, EpisodeURL: tvmazeMatchedURL(eps, q),
			Episodes: eps, FetchedAt: time.Now().UTC(),
		}, nil
	}

	// Absolute/Season==0: walk the show's seasons in air order (detail's
	// season list, specials excluded) and renumber absolute queries Nth-in-
	// order, exactly the tvmaze absolute discipline.
	nums := make([]int, 0, len(det.Seasons))
	for _, sn := range det.Seasons {
		if sn.SeasonNumber >= 1 {
			nums = append(nums, sn.SeasonNumber)
		}
	}
	sort.Ints(nums)
	if len(nums) > maxTMDBSeasons {
		nums = nums[:maxTMDBSeasons]
	}
	var out []Episode
	for _, n := range nums {
		if err := ctx.Err(); err != nil {
			return nil, s.redact(err)
		}
		eps, err := s.tmdbSeason(ctx, hit.ID, n)
		if err != nil {
			if tmdbMiss(err) {
				continue
			}
			return nil, err
		}
		out = append(out, eps...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		return out[i].Number < out[j].Number
	})
	if q.Absolute {
		for i := range out {
			out[i].Number = i + 1
		}
	}
	return &Media{
		Source: "tmdb", SourceID: strconv.Itoa(hit.ID),
		Kind: KindSeries, Title: firstNonEmpty(det.Name, hit.Name), Year: year,
		Artwork: tmdbArt(det.PosterPath), Synopsis: capSynopsis(stripHTML(det.Overview)),
		Genres: genres, Length: length, ShowURL: showURL, EpisodeURL: tvmazeMatchedURL(out, q),
		Episodes: out, FetchedAt: time.Now().UTC(),
	}, nil
}

// tmdbSeason fetches one season's episode table. A missing season (404) is
// a miss - the resolver then falls through rather than accepting another
// season's episodes positionally.
func (s *tmdbSource) tmdbSeason(ctx context.Context, showID, season int) ([]Episode, error) {
	resp, err := s.tmdbGet(ctx, "/3/tv/"+strconv.Itoa(showID)+"/season/"+strconv.Itoa(season), nil)
	if err != nil {
		return nil, err
	}
	var sr tmdbSeasonResp
	if err := tmdbDecode(resp, &sr, s.redact); err != nil {
		return nil, err
	}
	sn := sr.SeasonNumber
	if sn <= 0 {
		sn = season
	}
	out := make([]Episode, 0, len(sr.Episodes))
	for _, e := range sr.Episodes {
		if e.EpisodeNumber <= 0 {
			continue
		}
		esn := e.SeasonNumber
		if esn <= 0 {
			esn = sn
		}
		out = append(out, Episode{
			Season: esn, Number: e.EpisodeNumber,
			Title:    strings.TrimSpace(e.Name),
			Synopsis: capSynopsis(stripHTML(e.Overview)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Season != out[j].Season {
			return out[i].Season < out[j].Season
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

func (s *tmdbSource) findMovie(ctx context.Context, q Query) (*Media, error) {
	params := map[string]string{"query": q.Title}
	if q.Year > 0 {
		params["year"] = strconv.Itoa(q.Year)
	}
	resp, err := s.tmdbGet(ctx, "/3/search/movie", params)
	if err != nil {
		return nil, err
	}
	var sr tmdbMovieSearchResp
	if err := tmdbDecode(resp, &sr, s.redact); err != nil {
		if tmdbMiss(err) {
			return nil, nil
		}
		return nil, err
	}
	cands := make([]ShowCand, 0, len(sr.Results))
	kept := make([]tmdbMovieHit, 0, len(sr.Results))
	for _, h := range sr.Results {
		if h.ID == 0 || strings.TrimSpace(h.Title) == "" {
			continue
		}
		if q.Year > 0 {
			if y := dateYear(h.Release); y > 0 && y != q.Year {
				continue
			}
		}
		cands = append(cands, ShowCand{Name: h.Title, Weight: h.VoteCount})
		kept = append(kept, h)
	}
	winner, ok := pickBestShow(q.Title, cands)
	if !ok {
		return nil, nil
	}
	var hit tmdbMovieHit
	found := false
	for i, c := range cands {
		if c == winner {
			hit = kept[i]
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}

	resp, err = s.tmdbGet(ctx, "/3/movie/"+strconv.Itoa(hit.ID), nil)
	if err != nil {
		return nil, err
	}
	var det tmdbMovieDetail
	if err := tmdbDecode(resp, &det, s.redact); err != nil {
		if tmdbMiss(err) {
			return nil, nil
		}
		return nil, err
	}
	return &Media{
		Source: "tmdb", SourceID: strconv.Itoa(hit.ID),
		Kind: KindMovie, Title: firstNonEmpty(det.Title, hit.Title), Year: dateYear(det.Release),
		Artwork:   tmdbArt(det.PosterPath),
		Synopsis:  capSynopsis(stripHTML(det.Overview)),
		Genres:    tmdbGenreNames(det.Genres),
		Length:    runtimeLength(det.Runtime),
		MovieURL:  normalizeURL("https://www.themoviedb.org/movie/" + strconv.Itoa(hit.ID)),
		FetchedAt: time.Now().UTC(),
	}, nil
}

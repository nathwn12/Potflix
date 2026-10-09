package presence

import (
	"fmt"
	"strings"

	"potflix/internal/player"
	"potflix/internal/titleclean"
)

// cardspec.go - the Card view's spec model (pure logic, no UI).
//
// A CardSpec chooses what the card's two body lines say. Each line picks
// from a small fixed preset set (CardPresets) - there is no template
// language. The zero value falls back to the default on every line, so no
// migration is needed for stored configs.
//
// DefaultCardSpec reproduces today's shipped output BYTE-FOR-BYTE: its
// Render returns exactly BuildActivityResolved's Details/State for the same
// inputs (proven by TestCardSpecDefaultEquivalence). Non-default presets
// only reselect among the same already-capped strings; the paused
// " - Paused" marker path is preserved on every line.

// Details-line preset ids.
const (
	CardDetailsDefault = "default"
	CardDetailsTitle   = "title"
	CardDetailsEpisode = "episode"
	CardDetailsSeries  = "series"
)

// State-line preset ids.
const (
	CardStateDefault  = "default"
	CardStateTag      = "tag"
	CardStateSynopsis = "synopsis"
	CardStateNone     = "none"
)

// CardSpec is the stored choice of one preset per body line.
type CardSpec struct {
	Details string `json:"details,omitempty"`
	State   string `json:"state,omitempty"`
}

// CardPreset is one named choice in the CardPresets set.
type CardPreset struct {
	ID    string
	Label string
}

// DefaultCardSpec is today's shipped output: both lines on default.
func DefaultCardSpec() CardSpec {
	return CardSpec{Details: CardDetailsDefault, State: CardStateDefault}
}

// Normalized maps empty/unknown ids back to the default line, so a zero
// value (or a stored typo) can never reach the renderer.
func (s CardSpec) Normalized() CardSpec {
	out := s
	switch out.Details {
	case CardDetailsDefault, CardDetailsTitle, CardDetailsEpisode, CardDetailsSeries:
	default:
		out.Details = CardDetailsDefault
	}
	switch out.State {
	case CardStateDefault, CardStateTag, CardStateSynopsis, CardStateNone:
	default:
		out.State = CardStateDefault
	}
	return out
}

// IsDefault reports whether both lines are the shipped default.
func (s CardSpec) IsDefault() bool {
	n := s.Normalized()
	return n.Details == CardDetailsDefault && n.State == CardStateDefault
}

// CardPresets returns the small named set per line: 4 Details choices and
// 4 State choices. Labels stay short so the owner-drawn preset buttons
// ellipsize cleanly at any DPI.
func CardPresets() (details, states []CardPreset) {
	details = []CardPreset{
		{ID: CardDetailsDefault, Label: "Default"},
		{ID: CardDetailsTitle, Label: "Title"},
		{ID: CardDetailsEpisode, Label: "Episode"},
		{ID: CardDetailsSeries, Label: "Series"},
	}
	states = []CardPreset{
		{ID: CardStateDefault, Label: "Default"},
		{ID: CardStateTag, Label: "Tag"},
		{ID: CardStateSynopsis, Label: "Synopsis"},
		{ID: CardStateNone, Label: "Empty"},
	}
	return details, states
}

// isIdleTitle mirrors BuildActivity's idle gate: the title carries no media
// name. The idle card ("Browsing"/"Idle") is never restyled by presets.
func isIdleTitle(rawTitle string) bool {
	raw := rawTitle
	if strings.HasSuffix(raw, player.Suffix) {
		raw = raw[:strings.LastIndex(raw, player.Suffix)]
	}
	return strings.TrimSpace(raw) == "" || raw == player.AppName
}

// episodeTagOnly renders the filename episode tag with the paused marker
// path preserved (paused with no tag reads "Paused", as BuildActivity does).
func episodeTagOnly(rawTitle string, a *Action) string {
	raw := rawTitle
	if strings.HasSuffix(raw, player.Suffix) {
		raw = raw[:strings.LastIndex(raw, player.Suffix)]
	}
	var tagRaw string
	if idle := strings.TrimSpace(raw) == "" || raw == player.AppName; !idle {
		tagRaw = titleclean.Clean(raw).EpisodeTag
	}
	tagT := rpText(tagRaw, 128)
	if a != nil && a.Paused {
		return WithStateTime(tagT, pausedMarker)
	}
	return tagT
}

// synopsisOnly renders the resolved synopsis with the paused marker path
// preserved (paused with no synopsis still reads "Paused").
func synopsisOnly(syn string, a *Action) string {
	full := rpText(syn, 1<<28)
	if full == "" {
		if a != nil && a.Paused {
			return WithStateTime("", pausedMarker)
		}
		return ""
	}
	if a != nil && a.Paused {
		return WithStateTime(trimWord(full, 128-len(pausedMarker)), pausedMarker)
	}
	return trimWord(full, 128)
}

// Render returns the (details, state) body lines for the given inputs under
// this spec. The default spec returns BuildActivityResolved's lines exactly;
// every other preset reselects among the same capped strings. The idle card
// keeps its shipped lines under any spec.
func (s CardSpec) Render(rawTitle string, a *Action, r Resolved) (details, state string) {
	base := BuildActivityResolved(rawTitle, a, r)
	if isIdleTitle(rawTitle) {
		return base.Details, base.State
	}
	spec := s.Normalized()
	details, state = base.Details, base.State
	switch spec.Details {
	case CardDetailsTitle:
		// The header title: canonical series when known, else the
		// filename-derived header (base.Name), else today's line.
		switch {
		case r.Series != "":
			details = titleclean.Trunc(r.Series, 128)
		case base.Name != "":
			details = base.Name
		}
	case CardDetailsEpisode:
		if r.EpisodeName != "" {
			details = titleclean.Trunc(r.EpisodeName, 128)
		}
	case CardDetailsSeries:
		if r.Series != "" {
			if r.IsMovie && r.Year > 0 {
				details = titleclean.Trunc(fmt.Sprintf("%s (%d)", r.Series, r.Year), 128)
			} else {
				details = titleclean.Trunc(r.Series, 128)
			}
		}
	}
	switch spec.State {
	case CardStateTag:
		state = episodeTagOnly(rawTitle, a)
	case CardStateSynopsis:
		state = synopsisOnly(r.Synopsis, a)
	case CardStateNone:
		state = ""
	}
	return details, state
}

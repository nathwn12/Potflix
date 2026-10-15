//! `metadata::yearpref` -- the bounded year preference shared by the search
//! picks (Cinemeta, Tenrai, Kitsu, TVmaze).
//!
//! Rule: at equal title strength, a candidate carrying the query's year
//! outranks one that does not. "Equal strength" means the challenger fully
//! covers the query title (every query token present in the candidate name or
//! any canonical alias),
//! so a rip-year typo can never drag in an unrelated entity, and a candidate
//! with no year is neither preferred nor penalised (only `year == query_year`
//! counts as a match).
//!
//! The preference is SOFT: it only swaps one complete answer for another and
//! never returns a miss. A year mismatch never rejects a provider holding the
//! essentials (cover, title, episode, synopsis); year is a nice-to-have.
//!
//! Identity outranks coverage only through a foreign alias: a winner the
//! query names ONLY in translation (exact through a verified alias while the
//! display shares no title with the query) only yields to another exact
//! identity -- or to a year-qualified exact alias (`Hagane no
//! Renkinjutsushi (2009)` for a `Hagane no Renkinjutsushi`/2009 query: the
//! provider itself records that entry AS the query in the query year) --
//! never to a merely covering subtitle-carrier. When the query covers
//! the winner's display (the same title in longer form, or the display
//! string outright), a same-named continuation carrying the query year is the
//! year disambiguating, and the plain coverage rule applies.
//!
//! When the title winner contradicts an explicit query year through no
//! identity at all (the chain skips such a pick outright) AND its own record
//! is bare, the year-carriers rank among themselves by title with no
//! coverage floor: the provider returned them for this query and the year
//! agrees, so the title-strongest -- server order breaking a below-floor tie
//! -- takes the pick instead of conceding the leg. A described winner stands
//! even in contradiction (its record is evidence the year alone must not
//! overturn; the chain skips it and later legs decide). An exact winner is
//! never displaced this way: a wrong file year must not turn the correct
//! match into a miss, so only a year-agreeing identity ever takes its place.
//!
//! The real ranking stays in [`pick_best_show`]; this module only decides
//! whether a year-matching challenger may take the already-picked winner's
//! place.

use metadata_core::match_::{ShowCand, normalize_title, pick_best_show};

/// Lowercase alphanumeric tokens (punctuation folds to spaces, like the
/// matcher's own normalization, minus accent folding -- so this check is
/// conservative: it may decline a swap, never force a wrong one). Shared with
/// [`crate::anilist`] so corroboration tokens and coverage tokens agree.
pub(crate) fn tokens(s: &str) -> Vec<String> {
    s.to_lowercase()
        .chars()
        .map(|c| if c.is_alphanumeric() { c } else { ' ' })
        .collect::<String>()
        .split_whitespace()
        .map(str::to_string)
        .collect()
}

/// Exact title identity: the normalized query equals the candidate's display
/// name or one of its verified aliases. This is the same normalized equality
/// [`pick_best_show`] scores as 1.0, so the year preference below and the
/// chain's year gate agree with the matcher on what "exact" means. Only
/// canonical, translated and abbreviation aliases count: a provider whose
/// match aliases carry unverified synonyms passes a canonical-only view (see
/// [`covers_candidate`]), so an arbitrary synonym can never establish
/// identity here either.
pub(crate) fn is_exact_identity(query: &str, name: &str, aliases: &[String]) -> bool {
    let want = normalize_title(query);
    if want.is_empty() {
        return false;
    }
    if normalize_title(name) == want {
        return true;
    }
    aliases
        .iter()
        .any(|alias| !alias.trim().is_empty() && normalize_title(alias) == want)
}

/// Year-qualified exact identity: the normalized query title with the query
/// year attached equals the candidate's display name or one of its verified
/// aliases (`Hagane no Renkinjutsushi (2009)` normalizes to `hagane no
/// renkinjutsushi 2009`). A provider records a remake AS the shared title in
/// its year this way, while a same-stem spin-off carries no such alias --
/// so this is provider-supplied identity evidence, not a title guess, and it
/// opens the foreign-alias door exactly where a remake needs it. Reads the
/// same alias view as [`is_exact_identity`]: a caller passing a
/// canonical-only view (AniList) keeps unverified synonyms out here too.
pub(crate) fn is_year_qualified_identity(
    query: &str,
    query_year: i32,
    name: &str,
    aliases: &[String],
) -> bool {
    if query_year <= 0 {
        return false;
    }
    let want_title = normalize_title(query);
    if want_title.is_empty() {
        return false;
    }
    let want = format!("{want_title} {query_year}");
    if normalize_title(name) == want {
        return true;
    }
    aliases
        .iter()
        .any(|alias| !alias.trim().is_empty() && normalize_title(alias) == want)
}

/// Full query-token coverage: every query token appears in the candidate.
/// The candidate covers the query when its display name OR any canonical
/// alias covers every query token, so an English display name can never hide
/// a romaji alias the query names (Sailor Moon Crystal judged by `Pretty
/// Guardian Sailor Moon Crystal` alone fails a romaji query its `Bishoujo
/// Senshi Sailor Moon Crystal` alias covers). Only canonical titles count:
/// providers whose match aliases carry unverified synonyms (AniList) pass a
/// canonical-only view, so an arbitrary synonym can never authorize a year
/// override for an unrelated entry (`Monster`/2019 must keep the 2004 series
/// `19`, not the `Golden Kamuy` OVA `124756` whose synonym is `Monster`).
fn covers_candidate(query: &str, cand: &ShowCand) -> bool {
    std::iter::once(cand.name.as_str())
        .chain(cand.aliases.iter().map(String::as_str))
        .any(|title| covers_title(query, title))
}

/// Full query-token coverage: every query token appears in the title.
fn covers_title(query: &str, candidate: &str) -> bool {
    let q = tokens(query);
    if q.is_empty() {
        return false;
    }
    let c = tokens(candidate);
    q.iter().all(|t| c.contains(t))
}

/// The index to take: `winner`, unless `query_year > 0`, the winner's own
/// year disagrees, and some other candidate both carries the query year and
/// fully covers the query title -- then the best such challenger (ranked by
/// [`pick_best_show`] over the year-matching subset, so weight/popularity
/// still breaks ties among them).
///
/// A foreign-alias-exact winner additionally yields to a year-qualified exact
/// alias (see [`is_year_qualified_identity`]): the provider itself records a
/// remake AS the shared title in the query year, which a same-stem spin-off
/// never carries.
///
/// When the winner contradicts the query year through no identity at all --
/// neither display-exact nor alias-exact, so the chain would skip the pick
/// outright -- the year-carriers rank among themselves by title with no
/// coverage floor (ranked by [`pick_best_show`], server order breaking a
/// below-floor tie): the provider returned them for this query and the year
/// agrees, so the title-strongest -- but only when the winner's own record
/// is bare -- takes the pick instead of conceding the leg. A described
/// winner stands even in contradiction (its record is evidence the year
/// alone must not overturn; the chain skips it and later legs decide), and
/// an exact winner is never displaced this way. Providers without
/// search-time synopses pass `false` (unknown reads as bare: an undescribed
/// winner carries no record evidence to protect).
///
/// `complete` parallels `cands`/`years`: per-candidate record completeness
/// at pick time (a non-empty synopsis, description or summary in the search
/// data). A length mismatch with `cands` keeps `winner`, never a guess.
///
/// Identity outranks coverage only through a foreign alias: when the winner
/// IS the query solely through a verified alias AND the query does not even
/// cover the winner's display title (it names the work only in translation
/// -- `Shingeki no Kyojin` for `Attack on Titan`), a challenger that merely
/// covers the query but carries extra distinguishing tokens is a different
/// work and must not displace it merely because its year agrees (live:
/// `Shingeki no Kyojin`/2014 keeps the 2013 main series instead of the 2014
/// `Kuinaki Sentaku` spin-off -- romaji subtitles mark spin-offs sharing the
/// stem); only another exact identity -- or a year-qualified exact alias --
/// may take such a winner's place on the year. When the query DOES cover
/// the winner's display -- the same title in
/// longer form (`Bishoujo Senshi Sailor Moon` for `Sailor Moon`) or the
/// display string outright (`Dragon Ball Super`) -- a same-named
/// continuation carrying the query year is the year disambiguating, not a
/// hijack (live: the 2014 `Crystal` remake takes a `Bishoujo Senshi Sailor
/// Moon`/2014 query from the 1992 series; the 2026 `Beerus` entity takes a
/// `Dragon Ball Super`/2026 query from the 2015 series), so the plain
/// coverage rule applies.
pub(crate) fn prefer_year_match(
    query: &str,
    query_year: i32,
    cands: &[ShowCand],
    years: &[i32],
    complete: &[bool],
    winner: usize,
) -> usize {
    if query_year <= 0
        || winner >= cands.len()
        || cands.len() != years.len()
        || cands.len() != complete.len()
    {
        return winner;
    }
    if years[winner] == query_year {
        return winner;
    }
    // Blocked exactly when the winner's identity rests wholly on a foreign
    // alias: exact through an alias while the display shares no title with
    // the query. Every other shape -- display-exact, same-family, or plain
    // non-exact -- keeps the coverage rule.
    let winner_foreign_exact = {
        let cand = &cands[winner];
        let want = normalize_title(query);
        !want.is_empty()
            && normalize_title(&cand.name) != want
            && is_exact_identity(query, &cand.name, &cand.aliases)
            && !covers_title(&cand.name, query)
    };
    let mut sub: Vec<ShowCand> = Vec::new();
    let mut back: Vec<usize> = Vec::new();
    for (i, cand) in cands.iter().enumerate() {
        if years[i] != query_year {
            continue;
        }
        // A foreign-alias-exact winner yields only to exact identity or to a
        // year-qualified exact alias (a remake recorded AS the shared title
        // in the query year); anything else keeps the coverage rule.
        if winner_foreign_exact {
            if !is_exact_identity(query, &cand.name, &cand.aliases)
                && !is_year_qualified_identity(
                    query,
                    query_year,
                    &cand.name,
                    &cand.aliases,
                )
            {
                continue;
            }
        } else if !covers_candidate(query, cand) {
            continue;
        }
        sub.push(cand.clone());
        back.push(i);
    }
    if !sub.is_empty()
        && let Some(alt) = pick_best_show(query, &sub)
        && let Some(at) = sub.iter().position(|c| *c == alt)
    {
        return back[at];
    }
    // The coverage/exact doors found no year taker. When the winner itself
    // contradicts the query year through no identity at all AND its own
    // record is bare, the chain skips the pick outright -- so the
    // year-carriers rank among themselves with no coverage floor instead of
    // conceding the leg. A described winner stands (its record is evidence;
    // the chain skips it and later legs decide), and an exact winner never
    // reaches here: a wrong file year must not displace the correct match,
    // only a year-agreeing identity ever takes its place (handled above).
    if years[winner] > 0
        && !complete[winner]
        && !is_exact_identity(query, &cands[winner].name, &cands[winner].aliases)
    {
        let mut carriers: Vec<ShowCand> = Vec::new();
        let mut back: Vec<usize> = Vec::new();
        for (i, cand) in cands.iter().enumerate() {
            if years[i] != query_year {
                continue;
            }
            carriers.push(cand.clone());
            back.push(i);
        }
        if !carriers.is_empty() {
            if let Some(alt) = pick_best_show(query, &carriers)
                && let Some(at) = carriers.iter().position(|c| *c == alt)
            {
                return back[at];
            }
            // Every carrier scores below the title floor: the provider still
            // returned them for this query and the year agrees, so its own
            // relevance order (the subset keeps server order) decides.
            return back[0];
        }
    }
    winner
}

#[cfg(test)]
mod yearpref_tests {
    use super::{is_exact_identity, is_year_qualified_identity, prefer_year_match};
    use metadata_core::match_::ShowCand;

    fn cand(name: &str, aliases: &[&str]) -> ShowCand {
        ShowCand {
            name: name.to_string(),
            weight: 0,
            episode_count: 0,
            aliases: aliases.iter().map(|alias| alias.to_string()).collect(),
        }
    }

    /// Identity is the display name or a verified alias -- never a mere
    /// cover, and never an empty query.
    #[test]
    fn exact_identity_is_display_or_verified_alias() {
        assert!(is_exact_identity("Monster", "Monster", &[]));
        assert!(is_exact_identity(
            "Shingeki no Kyojin",
            "Attack on Titan",
            &["Shingeki no Kyojin".to_string()]
        ));
        assert!(!is_exact_identity(
            "Shingeki no Kyojin",
            "Attack on Titan: No Regrets",
            &["Shingeki no Kyojin: Kuinaki Sentaku".to_string()]
        ));
        assert!(!is_exact_identity("", "Monster", &[]));
        assert!(!is_exact_identity("Monster", "", &[]));
    }

    /// An exact-identity winner keeps its place against a covering
    /// challenger carrying the query year: the subtitle names a different
    /// work (live: the 2014 `Kuinaki Sentaku` spin-off must not take a
    /// `Shingeki no Kyojin`/2014 query from the 2013 main series).
    #[test]
    fn exact_winner_keeps_over_covering_challenger() {
        let cands = vec![
            cand("Attack on Titan", &["Shingeki no Kyojin"]),
            cand(
                "Attack on Titan: No Regrets",
                &["Shingeki no Kyojin: Kuinaki Sentaku"],
            ),
        ];
        assert_eq!(
            prefer_year_match("Shingeki no Kyojin", 2014, &cands, &[2013, 2014], &[true, true], 0),
            0,
            "an exact winner never yields to a covering subtitle-carrier"
        );
    }

    /// An exact-identity winner still yields to another exact identity
    /// carrying the query year: the year breaks ties among identities.
    #[test]
    fn exact_winner_yields_to_exact_challenger() {
        let cands = vec![cand("Monster", &[]), cand("Monster", &[])];
        assert_eq!(
            prefer_year_match("Monster", 2019, &cands, &[2004, 2019], &[true, true], 0),
            1,
            "the year breaks ties among exact identities"
        );
    }

    /// A display-exact winner keeps the coverage rule: the query names that
    /// display string outright, so a same-named continuation carrying the
    /// query year is the year disambiguating, not a hijack (live: `Dragon
    /// Ball Super`/2026 takes the 2026 `Beerus` entity over the 2015
    /// series).
    #[test]
    fn display_exact_winner_still_swaps_on_cover() {
        let cands = vec![
            cand("Dragon Ball Super", &[]),
            cand("Dragon Ball Super: Beerus", &[]),
        ];
        assert_eq!(
            prefer_year_match("Dragon Ball Super", 2026, &cands, &[2015, 2026], &[true, true], 0),
            1,
            "a display-exact winner yields to a year-agreeing continuation"
        );
    }

    /// A same-family winner keeps the coverage rule too: the query covers
    /// the winner's display (the same title in longer form), so a
    /// same-named continuation carrying the query year is the year
    /// disambiguating (live: `Bishoujo Senshi Sailor Moon`/2014 takes the
    /// 2014 `Crystal` remake over the 1992 series).
    #[test]
    fn same_family_winner_still_swaps_on_cover() {
        let cands = vec![
            cand("Sailor Moon", &["Bishoujo Senshi Sailor Moon"]),
            cand(
                "Pretty Guardian Sailor Moon Crystal",
                &["Bishoujo Senshi Sailor Moon Crystal"],
            ),
        ];
        assert_eq!(
            prefer_year_match("Bishoujo Senshi Sailor Moon", 2014, &cands, &[1992, 2014], &[true, true], 0),
            1,
            "a same-family winner yields to a year-agreeing continuation"
        );
    }

    /// A non-exact winner keeps the old coverage rule: a fully covering
    /// challenger carrying the query year still takes its place.
    #[test]
    fn covering_winner_still_swaps_on_cover() {
        let cands = vec![cand("Fate/Zero", &[]), cand("Fate/stay night", &[])];
        assert_eq!(
            prefer_year_match("Fate", 2006, &cands, &[2011, 2006], &[true, true], 0),
            1,
            "coverage still decides among non-identities"
        );
    }

    /// A foreign-alias-exact winner yields to a year-qualified exact alias:
    /// the provider records the remake AS the shared title in the query year
    /// (live Kitsu: `Hagane no Renkinjutsushi (2009)` rides on `3936`, so a
    /// `Hagane no Renkinjutsushi`/2009 query takes the 2009 `Brotherhood`
    /// over the 2003 series).
    #[test]
    fn foreign_exact_winner_yields_to_year_qualified_alias() {
        let cands = vec![
            cand("Fullmetal Alchemist", &["Hagane no Renkinjutsushi"]),
            cand(
                "Fullmetal Alchemist: Brotherhood",
                &[
                    "Hagane no Renkinjutsushi: Fullmetal Alchemist",
                    "Hagane no Renkinjutsushi (2009)",
                ],
            ),
        ];
        assert_eq!(
            prefer_year_match("Hagane no Renkinjutsushi", 2009, &cands, &[2003, 2009], &[true, true], 0),
            1,
            "a year-qualified alias opens the foreign-alias door"
        );
    }

    /// The year-qualified door stays shut without the alias: a covering
    /// subtitle-carrier must not take a foreign-alias-exact winner's place
    /// merely because its year agrees (live Kitsu: `Shingeki no Kyojin:
    /// Kuinaki Sentaku` carries no year-qualified alias, so `Shingeki no
    /// Kyojin`/2014 keeps the 2013 main series).
    #[test]
    fn foreign_exact_winner_keeps_without_year_qualified_alias() {
        let cands = vec![
            cand("Attack on Titan", &["Shingeki no Kyojin"]),
            cand(
                "Attack on Titan: No Regrets",
                &["Shingeki no Kyojin: Kuinaki Sentaku"],
            ),
        ];
        assert_eq!(
            prefer_year_match("Shingeki no Kyojin", 2014, &cands, &[2013, 2014], &[false, false], 0),
            0,
            "coverage alone never opens the foreign-alias door"
        );
        assert!(!is_year_qualified_identity(
            "Shingeki no Kyojin",
            2014,
            "Attack on Titan: No Regrets",
            &["Shingeki no Kyojin: Kuinaki Sentaku".to_string()],
        ));
        assert!(!is_year_qualified_identity("Hagane no Renkinjutsushi", 0, "x", &[]));
        assert!(!is_year_qualified_identity("", 2009, "x", &[]));
    }

    /// A winner the chain would skip outright (contradicts the query year
    /// through no identity) concedes to the year-carriers ranked by title:
    /// a below-floor tie breaks in server order (live: `Kimi no Na
    /// wa`/1953 takes the 1953 `What Is Your Name?` over the 1955
    /// `shison-tachi` title winner, which shares no title with the query).
    #[test]
    fn skipped_winner_yields_to_year_carriers() {
        let cands = vec![
            cand("Kimi no na wa no shison-tachi", &[]),
            cand("What Is Your Name?", &[]),
            cand("What Is Your Name? Part II", &[]),
        ];
        assert_eq!(
            prefer_year_match("Kimi no Na wa", 1953, &cands, &[1955, 1953, 1953], &[false, false, false], 0),
            1,
            "a skipped winner concedes to the year-carriers"
        );
    }

    /// No year-carrier means no concession: the title winner stands and the
    /// chain skips it as before (live: `Kimi no Na wa`/2015 keeps the
    /// title winner, which the chain then skips on the year).
    #[test]
    fn skipped_winner_with_no_year_carrier_stays() {
        let cands = vec![
            cand("Kimi no na wa no shison-tachi", &[]),
            cand("Your Name.", &[]),
        ];
        assert_eq!(
            prefer_year_match("Kimi no Na wa", 2015, &cands, &[1955, 2016], &[false, true], 0),
            0,
            "without a year-carrier the winner stands"
        );
    }

    /// An exact winner is never displaced by a year-carrier: a wrong file
    /// year must not turn the correct match into a miss (live AniList
    /// canonical view: `Monster`/2019 keeps the 2004 series `19`, never the
    /// 2019 `Golden Kamuy` OVA whose `Monster` synonym the canonical-only
    /// view withholds).
    #[test]
    fn exact_winner_never_yields_to_year_carrier() {
        let cands = vec![
            cand("Monster", &[]),
            cand("Golden Kamuy 2nd Season OVA", &[]),
        ];
        assert_eq!(
            prefer_year_match("Monster", 2019, &cands, &[2004, 2019], &[true, false], 0),
            0,
            "an exact winner never concedes on the year"
        );
    }

    /// A described winner stands even in contradiction: its record is
    /// evidence the year alone must not overturn, so the pick keeps it and
    /// the chain skips it for later legs to decide (live TVmaze: `Space
    /// Battleship Yamato`/1974 keeps the described 2012 `Star Blazers:
    /// Space Battleship Yamato` title winner instead of conceding to the
    /// year-carrying `Star Blazers` -- the 1974 work still resolves through
    /// the corroborated later leg).
    #[test]
    fn described_skipped_winner_stays() {
        let cands = vec![
            cand("Star Blazers: Space Battleship Yamato", &[]),
            cand("Star Blazers", &[]),
        ];
        assert_eq!(
            prefer_year_match(
                "Space Battleship Yamato",
                1974,
                &cands,
                &[2012, 1974],
                &[true, true],
                0
            ),
            0,
            "a described winner never concedes on the year"
        );
    }
}

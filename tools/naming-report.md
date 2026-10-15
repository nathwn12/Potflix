# Potflix cleaner - naming report (sample4)

Measurement of `clean()` / `stripTags()` in
`src/main/java/me/potflix/rpc/Main.java` against a media library.

Method: enumerating the configured library roots only (e.g. `<library-root-1>`,
`<library-root-2>`), sampling every 3rd video file,
dumping `{title, episodeTag, episodeName, display}` via reflection
(`BatchDump`), then classifying. **No file under the library roots was modified.**
`Main.java` was not edited; the running app was not touched.

## 1. Files scanned

| Metric | Count |
|---|---:|
| **Total enumerated** | **7810** |
| **Sampled (every 3rd) -> tested** | **2604** (written to gitignored `tools/titles-sample4.txt`) |

## 2. Results

| Metric | Count |
|---|---:|
| Filenames tested | 2604 |
| **Genuine failures** | **397 (15.2%)** |
| Heuristic flags that were actually correct (false positives) | 26 |
| Full-clean (no defect) | 2181 |

Genuine failure = produced `display`/`title` is **wrong**, not merely long:
leftover scene noise, an episode number present but not extracted (or extracted
from the wrong token), a garbled/truncated head, or a connector word left dangling.
Long-but-correct episode names, legit all-caps song/OP-ED names, and multi-word
episode names were **not** counted.

## 3. Root causes (by frequency)

| # | Root cause | Failures |
|---|---|---:|
| RC1 | **Episode token not extracted** — leading `SxxExx` with no series name, headless `Episode NN` / `ED NN` / `OP NN`, `NNxNN`, `SxxExxExx` multi-episode, `S1950E04`, trailing `NN` | 266 |
| RC2 | **Codec/source leftovers** — `H264`/`H265`, standalone `5.1`/`7.1`, `NF`, `AMZN`, `DUAL`, `MSubs`, `IMAX`, `MA`, `SDR`, `vmaf95`, `HDR10+`, `AAC5`, `Multi3` | 47 |
| RC3 | **Connector word / `Season N` misparse** — title keeps dangling `Season`/`Chapter`/`Episode`, and `Season NN` or `Season NN NCOP` is parsed as an episode | 73 |
| RC4 | **Over-strip of legit words** — bare `extended`, `theatrical`, `final` removed from real titles | 14 |
| RC5 | **Movie `Part/Chapter N` stolen as episode** — `John Wick Chapter 1`, `Mockingjay Part 2` | 2 |
| RC7 | **`NCOP`/`NCED` blocked by a trailing separator** left by the parenthetical removal | 2 |

(The per-line total is 404; 7 lines carry two causes → **397 unique**.)

## 4. Representative failures (raw → produced display → defect → fix)

### RC1 — episode token not extracted
| Raw filename | Produced display | What's wrong | Proposed fix |
|---|---|---|---|
| `S01E03-The Rookie Fire Soldier Games [0CCBF9D4].mkv` | `S01E03-The Rookie Fire Soldier Games` | `S01E03` never extracted (pattern needs a series head); raw token left in title. 194 files like this | SERIES: allow empty head |
| `Episode 104.mkv` | `Episode 104` | `Episode NN` not recognised (ALT_EP needs a non-empty tail) | new EPWORD rule |
| `ED 03.mkv` / `OP 02.mkv` | `ED 03` / `OP 02` | headless OP/ED not recognised (NCBOOK needs a head) | NCBOOK: allow empty head + display fallback |
| `The Amazing World of Gumball S02E13E14  The Skull - The Bet.mkv` | `The Amazing World of Gumball S02E13E14 The Skull - The Bet` | double-episode token unmatched; whole token kept | new MULTI rule |
| `Regular Show (2010) - S03E30-E31 - …` | `Regular Show - S3 E30 "E31 - …"` | second episode leaks into `episodeName` (30 files) | same MULTI rule |
| `[Kosaka] Serial Experiments Lain - 01x09 - Layer09 PROTOCOL.mkv` | `… Lain - 01x09 - Layer09 PROTOCOL` | `NNxNN` format unrecognised | new XEP rule |
| `[Anime Time] Nichijou 04.mkv` | `Nichijou 04` | trailing episode number has no tail → not extracted | new TRAIL rule |
| `[Trix] Shingeki no Kyojin S1950E04 …` (Tom & Jerry) | `Tom and Jerry - S1950E04 - …` | 4-digit season fails `\d{1,2}` | widen season group to `\d{1,4}` |

### RC2 — codec/source leftovers
| Raw filename | Produced display | What's wrong | Proposed fix |
|---|---|---|---|
| `The.Boys.S05E04.1080p.WEB.h264-ETHEL.mkv` | `The Boys - S5 E4 "WEB h264"` | `h264` absent from TAGS | add `h\.?26[45]` to TAGS |
| `The.Legend.of.Korra.S02E01…DDP.5.1.H.265.-EDGE2020.mkv` | `… S2 E1 "Rebel Spirit H 265"` | `H 265` / `5 1` survive | add `h\.?26[45]`, `5[ .\-]?1`, `7[ .\-]?1` |
| `The.Dictator.2012.UNRATED.1080p.BluRay.AV1.Opus.5.1-RAV1NE.mkv` | `The Dictator 2012 5 1` | standalone `5.1`; group `-RAV1NE` not dropped | add channels tag + `av1\|opus` to SCENE_HINT |
| `BAKI-DOU…S01E02…DDP5.1.H.264.MSubs-ToonsHub.mkv` | `… S1 E2 "THE BEAT NF DUAL H 264 MSubs"` | `NF`, `DUAL`, `MSubs`, `H 264` left in episode name | add `nf\|amzn\|dual\|msubs` + codec tags |
| `Avengers - Endgame (2019) IMAX (1080p…).mkv` | `Avengers - Endgame IMAX` | `IMAX` not a tag | add `imax` to TAGS |
| `War.Dogs.2016…AAC5.1-[YTS.MX].mkv` | `War Dogs 2016 AAC5 1` | `AAC5.1` not matched | broaden AAC/DD patterns to `aac\d?(?:[ .\-]?\d)?` |
| `Arcane.S01E03…HDR10+.AV1-SomniWare.mkv` | `Arcane - S1 E3 "NF +"` | `HDR10+` leaves a bare `+` | add `hdr\d*\+?` and trim stray `+` |

### RC3 — connector word / `Season N` misparse
| Raw filename | Produced display | What's wrong | Proposed fix |
|---|---|---|---|
| `Season 03 - Stop-Motion Lego Intro.mkv` | `Season — E3 "Stop-Motion Lego Intro"` | `Season NN` treated as an episode; title is just "Season" | exclude connector-headed ALT_EP; strip `Season NN -` prefix |
| `Arcane. Season 01. Episode 05. By Wild_Cat.mkv` | `Arcane Season — E1 "Episode 05 By Wild Cat"` | season taken as episode, real episode left in name | new SEASON_EP rule → `S1 E5` |
| `Monster - Chapter 01 - Herr Dr. Tenma.mkv` | `Monster - Chapter — E1 "Herr Dr Tenma"` | "Chapter" left dangling in title | new CHAPTER rule (no 4-digit year) |
| `[DB]Vinland Saga Season 2_-_NCOP01_…` | `Vinland Saga Season — E2` | season parsed as episode; NCOP missed | SEASON + NCOP fixes |
| `Gurren Lagann Parallel Works - Episode 6 (CBB).mkv` | `Gurren Lagann Parallel Works - Episode — E6` | "Episode" left in title | new EPWORD rule removes connector |

### RC4 — over-strip of legit words
| Raw filename | Produced display | What's wrong | Proposed fix |
|---|---|---|---|
| `Deleted and Extended Scenes.mkv` | `Deleted and Scenes` | bare `extended` in TAGS removes a real word | TAGS: `extended` → `extended[ .\-]?cut` |
| `Original Japanese Theatrical Trailers.mkv` | `Original Japanese Trailers` | bare `theatrical` removed | TAGS: `theatrical` → `theatrical[ .\-]?cut` |
| `… Gurren Lagann - 23 - Let's Go, The Final Battle…` | `… E23 "Let's Go, The Battle"` | bare `final` removed from an episode name | remove bare `final` from TAGS |

### RC5 / RC7
| Raw filename | Produced display | What's wrong | Proposed fix |
|---|---|---|---|
| `John Wick Chapter 1 2014 1080p BluRay HEVC x265 5.1 BONE.mkv` | `John Wick Chapter — E1 "2014 5 1 BONE"` | movie "Chapter 1" stolen as episode | CHAPTER only when no year; ALT_EP rejects a year-leading tail |
| `…Mockingjay.Part.2.2015…mkv` | `The Hunger Games Mockingjay Part — E2 "2015 …"` | movie "Part 2" stolen as episode | ALT_EP rejects head ending in `Part/Chapter/Season` |
| `[DB]Saiki Kusuo no Psi-nan_-_NCOP01_(10bit…).mkv` | `Saiki Kusuo no Psi-nan - NCOP01` | `ds` keeps a trailing `_`, so NCBOOK's `$` anchor fails | strip trailing separators from `ds` |
| `[DB]Saiki Kusuo no Psi-nan 2_-_NCED02_…` | `Saiki Kusuo no Psi-nan — E2` | same trailing `_`; ALT_EP then grabs the season `2` | same `ds` fix (then NCBOOK → `ED 2`) |

## 5. Proposed changes (exact regex / rule edits)

All changes are to `clean()` / `stripTags()` / the Pattern constants in `Main.java`.

**P1 — strip trailing separators from `ds`** (line 228). Root cause of RC7.
```java
// old
String ds = s.replaceAll("\\([^)]*\\)", " ").replaceAll("\\s+", " ").trim();
// new
String ds = s.replaceAll("\\([^)]*\\)", " ").replaceAll("\\s+", " ").trim()
        .replaceAll("[. _\\-]+$", "");
```

**P2 — SERIES: allow an empty series head** (line 207). Fixes RC1 (`S01E03-…`).
```java
// old
(?i)^(.+?)[. _\-]+[Ss](\d{1,2})[ . _\-]*[Ee](\d{1,2})(?:[. _\-]+(.*))?$
// new
(?i)^(.*?)[. _\-]*[Ss](\d{1,2})[ . _\-]*[Ee](\d{1,3})(?:[. _\-]+(.*))?$
```
When `group(1)` is empty, derive `title` from `rest` (episode name) instead of the
whole string, e.g. `S01E03-The Rookie Fire Soldier Games` → `The Rookie Fire Soldier Games — S1 E3`.

**P3 — NEW `MULTI` (double episode), tested before SERIES** (new constant). Fixes RC1/RC6.
```java
static final Pattern MULTI = Pattern.compile(
  "(?i)^(.*?)[. _\\-]*[Ss](\\d{1,2})[ . _\\-]*[Ee](\\d{1,3})[ ._\\-]*[-\\u2013\\u2014]?[Ee](\\d{1,3})(?:[. _\\-]+(.*))?$");
// epTag = "S"+g2+" E"+g3+"-"+g4 ; title = stripTags(g1) ; rest = g5
```
Fixes `…S02E13E14 The Skull - The Bet` and `Regular Show S03E30-E31 …`.

**P4 — NEW `EPWORD` (Episode/Ep, optional head), before ALT_EP** (new constant). Fixes RC1/RC3.
```java
static final Pattern EPWORD = Pattern.compile(
  "(?i)^(.*?)[. _\\-]*(?:Episode|Ep)\\.?[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
// epTag = "E"+g2 ; title = stripTags(g1) ; rest = g3
```
Fixes `Episode 104`, `BLEACH Kai - Episode 38 - FEAR FOR FIGHT`,
`Gurren Lagann Parallel Works - Episode 6 (CBB)`.

**P5 — NEW `SEASON_EP` and `SEASON_EXTRA`** (new constants). Fixes RC3.
```java
static final Pattern SEASON_EP = Pattern.compile(
  "(?i)^(.*?)[. _\\-]*Season[ . _\\-]*(\\d{1,2})[. _\\-]+(?:Episode|Ep)\\.?[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
// -> S(g2) E(g3)
static final Pattern SEASON_EXTRA = Pattern.compile(
  "(?i)^(.*?)[. _\\-]*Season[ . _\\-]*(\\d{1,2})[. _\\-]+(.+)$");
// no episode number: drop the "Season NN -" connector, title = stripTags(g3)
```
Fixes `Arcane. Season 01. Episode 05. By Wild_Cat` → `Arcane — S1 E5 "By Wild Cat"`
and `Season 03 - Stop-Motion Lego Intro` → `Stop-Motion Lego Intro`.

**P6 — NEW `CHAPTER` (only when no 4-digit year)** (new constant). Fixes RC3/RC5.
```java
static final Pattern CHAPTER = Pattern.compile(
  "(?i)^(.+?)[. _\\-]+Chapter[ . _\\-]*(\\d{1,4})(?:[. _\\-]+(.*))?$");
// apply only if !s.matches(".*\\b(19|20)\\d{2}\\b.*")
// -> Monster - Chapter 01 - Herr Dr. Tenma  =>  Monster — E1 "Herr Dr Tenma"
```

**P7 — NEW `XEP` (`NNxNN`)** (new constant). Fixes RC1.
```java
static final Pattern XEP = Pattern.compile(
  "(?i)^(.*?)[. _\\-]+(\\d{1,2})x(\\d{2,3})(?:[. _\\-]+(.*))?$");
// -> S(g2) E(g3)
```

**P8 — NEW `TRAIL` (trailing episode number, conservative)** (new constant). Fixes RC1.
```java
static final Pattern TRAIL = Pattern.compile("(?i)^(.{3,}?)[. _\\-]+(0\\d{1,2}|\\d{3})$");
// only when !s.matches(".*\\b(19|20)\\d{2}\\b.*")  -> E(g2)
```
(The `0\d` / 3-digit guard avoids `Apollo 13`, `Ocean's 11`, `Se7en` false positives.)
Fixes `[Anime Time] Nichijou 04`, `Podcast 07`.

**P9 — ALT_EP guard: reject connector-headed heads and year tails** (around line 265).
Fixes RC3/RC5.
```java
// add to the existing if(...) condition:
&& !a.group(1).matches("(?i).*\\b(Season|Chapter|Part|Episode|Ep|Vol(ume)?)$")
&& !(a.group(3) != null && a.group(3).matches("^(19|20)\\d{2}\\b.*"))
```

**P10 — TAGS additions / corrections** (line 211).
```java
// ADD tokens
h\.?26[45] | 5[ .\-]?1 | 7[ .\-]?1 | nf | amzn | dsnp | atvp | hmax | imax | sdr | hlg
| multi\d* | msubs | dual | vmaf\d* | thai | tamil | hindi | kor | jpn | eng
// BROADEN audio/codec
aac\d?(?:[ .\-]?\d)?      (replaces aac2?\.?0?)
ddp?[ .\-]?\d(?:[ .\-]?\d)? (covers DDPA5 1, DD5.1)
hdr\d*\+?                  (replaces hdr\d*)
// REMOVE from the bare alternatives (keep only the *cut forms)
extended   -> extended[ .\-]?cut
theatrical -> theatrical[ .\-]?cut
final      -> (delete; too common in episode titles)
// keep unrated / proper / repack as-is (scene-only words)
```
Note: do **not** add bare `ma`; strip standalone `MA` only inside the scene-suffix
region (see P11) to avoid killing titles such as "Ma".

**P11 — SCENE_HINT additions** (line 212). Fixes RC2 group suffixes.
```java
// old: (1080p|2160p|720p|x26[45]|web[ .\-]?dl|bluray|bdrip|hdtv|dvdrip|hevc)
// new: add  |av1|opus|av1|4k|uhd
```
Makes `…Opus.5.1-RAV1NE` and `…h264-ETHEL` drop their `-GROUP` suffix.

**P12 — empty-title display fallback.** When a headless rule produces no title:
- if there is an episode name, use it as the title (`S01E03-…` case);
- if there is none, drop the `" — "` separator and show the tag alone
  (`ED 03` → `ED 3`, not `ED 3 — ED 3`).

## 6. Clean outputs worth keeping as regression samples

| Raw filename | Expected display after fixes |
|---|---|
| `Breaking.Bad.S02E08.Better.Call.Saul.1080p.BluRay.x265-CtrlHD.mkv` | `Breaking Bad — S2 E8 "Better Call Saul"` |
| `S01E03-The Rookie Fire Soldier Games [0CCBF9D4].mkv` | `The Rookie Fire Soldier Games — S1 E3` |
| `The Amazing World of Gumball S02E13E14  The Skull - The Bet.mkv` | `The Amazing World of Gumball — S2 E13-14 "The Skull - The Bet"` |
| `[Kosaka] Serial Experiments Lain - 01x09 - Layer09 PROTOCOL.mkv` | `Serial Experiments Lain — S1 E9 "Layer09 PROTOCOL"` |
| `Arcane. Season 01. Episode 05. By Wild_Cat.mkv` | `Arcane — S1 E5 "By Wild Cat"` |
| `Monster - Chapter 01 - Herr Dr. Tenma.mkv` | `Monster — E1 "Herr Dr Tenma"` |
| `[DB]Saiki Kusuo no Psi-nan_-_NCOP01_(10bit_BD1080p_x265).mkv` | `Saiki Kusuo no Psi-nan — OP 1` |
| `The.Boys.S05E04.1080p.WEB.h264-ETHEL.mkv` | `The Boys — S5 E4` |
| `Deleted and Extended Scenes.mkv` | `Deleted and Extended Scenes` |
| `[2ndfire] Tengen Toppa Gurren Lagann - 26 - Let's Go, Comrades! [145E4A7F].mkv` | `Tengen Toppa Gurren Lagann — E26 "Let's Go, Comrades!"` |

## 7. Not counted (explicitly excluded)
- Long-but-correct episode names and multi-word names.
- Legit all-caps song / OP-ED names.
- Movie titles that merely lost a parenthetical year (`X-Men (2000)` → `X-Men`) —
  arguably a gap, but the produced title is not *wrong*, so not counted.
- `Part N` / `Chapter N` that correctly belong to an already-correct episode name
  (these 26 were the false-positive flags).

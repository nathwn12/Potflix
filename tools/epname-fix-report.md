# Episode titles in the Discord presence — fix report

Status: **shipped and verified live** (2026-10-05).

## Symptom
The Rich Presence card showed only `S3 E6` — the episode title was dropped, so a
card could never read `S3 E6 "Sunset"`.

## Change — `src/main/java/me/potflix/rpc/Main.java`
Three edits, all inside the presence path:

1. **`clean()` — keep single-word episode titles.** The old rule discarded *every*
   episode-name tail that contained no space (`if (!epName.contains(" ")) epName = "";`),
   which is why `Sunset` vanished. Replaced with a metadata-only rejection:
   `if (epName.matches("(?i)(?:WEB|DS4K|v\\d+|\\d+|\\+)")) epName = "";`
2. **`clean()` — release-group suffixes and `OVA`.** A trailing hyphen-attached
   uppercase suffix is removed (`SHOW.S01E02.TITLE-GROUP.mkv` -> `TITLE`), and `OVA`
   is added to the metadata filter so it is not quoted as a title.
3. **`pushActivity()` — no duplicated card.** For headless files the episode tag was
   pushed as both `details` and `state`. When the truncated `context` equals
   `details`, `state` is now pushed empty.

## Evidence — 2,604 real library filenames (`tools/titles-sample4.txt`)
| metric | HEAD | fixed |
|---|---|---|
| episode tags changed | — | **0 / 2604** |
| card rows with identical `details`/`state` | 27 | **0** |
| episode titles added | — | **106** |
| episode titles removed | — | **2** (both release-group leftovers) |
| legitimate title text lost | — | **0** |

Reproduce with `tools/EpisodeNameDump.java` (or `tools/EpisodeVerifier2.java`)
compiled against `dist/Potflix.jar`. Two independent verifier lanes did this
separately and neither reused the author's dumper.

## Live proof (this machine, shipped jar)
```
connected clientId=1556352808964333628
push details=[Breaking Bad] state=[S3 E6 "Sunset"] ct=1948602 total=2833747
```
Before: `push details=[Breaking Bad] state=[S3 E6]`.

## Known limitation (documented, bounded)
The uppercase-suffix rule is a heuristic; syntax alone cannot separate a release
group from legitimate uppercase title text. On the 2,604-file sample it has
2 true positives and 0 false positives. Invented cases can still fool it
(`The Return-PART2` -> `The Return`, mixed-case `TITLE-Group` is kept whole).
Four sample rows are *pre-existing* movie/trailer misparses (e.g.
`Astrid's Whole Dragon Trilogy in 60 Seconds.mkv` -> `E60`); they now show an
extra quoted fragment because the name is retained. Fixing those needs movie
detection, which is out of scope for this change.

## Verification artifact archive
The raw verifier logs (four files, ~1.7 MB of dump) were kept out of the repo;
they live in `%TEMP%\potflix-evidence\` and are regenerable from the harnesses.

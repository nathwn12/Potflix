# Library/metadata benchmark baselines (measurement only)

Harness: `go/internal/metadata/bench_test.go` (parse/context/cache hot path)
and `go/internal/library/bench_test.go` (plan/scan/fetch hot path), plus
`go/internal/store/store_bench_test.go` (store put/load hot path).
No benchmark touches production code, the live cache dir
(`%LOCALAPPDATA%\Potflix\meta` - every cache is a `b.TempDir()`), or the
real network (empty/offline sources, in-process `fakeSource`, or loopback
`httptest` only). Corpus generators use fixed seeds (metadata corpus is a
fixed ~30-entry table; `benchPlanPaths(1, ...)`, warm seed 7), so before/after
runs compare the same input.

Exact command (from `go/`):

```powershell
go test -run '^$' -bench . -benchmem -count=3 ./internal/metadata/ ./internal/library/
```

Fast smoke (one iteration each, `ScanWarm` ~= 20 ms, `ScanCold` ~= 26 ms):

```powershell
go test -run '^$' -bench . -benchmem -benchtime=1x ./internal/metadata/ ./internal/library/
```

## Baseline - 2026-10-09 - `go1.27.0 windows/amd64` (AMD Ryzen 7 5800X)

Mean of `-count=3` (per-op; `ScanWarm`/`ScanCold` run N=1 per count):

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---|---|---|---|
| metadata `BenchmarkCleanTitle` | ~42,042 | ~1,570 | 61 | per corpus item (~30 titles) ~= 1.4 µs |
| metadata `BenchmarkResolveQuery` | ~105,414 | ~3,065 | 160 | filename + folder context |
| metadata `BenchmarkContextFromPath` | ~49,299 | ~1,150 | 75 | full-path folder walk |
| metadata `BenchmarkCacheHit` | ~486 | 184 | 7 | mem-hit steady state, offline + empty sources |
| library `BenchmarkBuildPlan7847` | ~829,918,117 | ~31,782,659 | ~1,578,248 | 7,847 paths / ~700 folders ~= 106 µs per path |
| library `BenchmarkScanWarm` | ~19,627,333,733 | ~512,963 | ~9,382 | 50 gated disk hits: the 400 ms-gate floor (49*400 ms = 19.6 s), zero fetches asserted |
| library `BenchmarkScanColdFakeProvider` | ~7,612,016,367 | ~497,677 | ~5,785 | 20 gated loopback-http fetches (19*400 ms = 7.6 s) |
| library `BenchmarkFetchAll1000` | ~843,011 | ~629,485 | ~7,069 | gate disabled (`gateInterval 0`); ~= 0.84 µs per group |

Raw `count=3` tails are archived in `go/bench-baseline.txt` (gitignored) and
`$env:TEMP\potflix-bench-baseline.txt`.

## After - 2026-10-09 - `go1.27.0 windows/amd64` (AMD Ryzen 7 5800X)

Change: global 400 ms scan gate removed from `library.Scan` (provider
throttling now lives in `metadata`'s per-host limiter, unknown hosts
ungated); `decodeCapped` drains up to 512 KB before Close (keep-alive
reuse). Benchmarks themselves untouched. Same exact command (from `go/`):

```powershell
go test -run '^$' -bench . -benchmem -count=3 ./internal/metadata/ ./internal/library/
```

Mean of `-count=3`:

| Benchmark | Before ns/op | After ns/op | Delta | After B/op | After allocs/op |
|---|---|---|---|---|---|
| metadata `BenchmarkCleanTitle` | ~42,042 | ~39,901 | ~= noise (-5%) | ~1,570 | 61 |
| metadata `BenchmarkResolveQuery` | ~105,414 | ~97,356 | ~= noise (-8%) | ~3,065 | 160 |
| metadata `BenchmarkContextFromPath` | ~49,299 | ~44,454 | ~= noise (-10%) | ~1,151 | 75 |
| metadata `BenchmarkCacheHit` | ~486 | ~424 | ~= noise (path untouched) | 184 | 7 |
| library `BenchmarkBuildPlan7847` | ~829,918,117 | ~821,709,600 | ~= noise (-1%) | ~31,788,719 | ~1,578,260 |
| library `BenchmarkScanWarm` | ~19,627,333,733 | ~20,401,331 | **~962x faster** (19.6 s -> ~20 ms) | ~424,256 | ~9,223 |
| library `BenchmarkScanColdFakeProvider` | ~7,612,016,367 | ~26,207,692 | **~290x faster** (7.6 s -> ~26 ms) | ~352,459 | ~5,591 |
| library `BenchmarkFetchAll1000` | ~843,011 | ~908,294 | ~= noise (+8%; `FetchAll` code path untouched, stub + `gateInterval 0`) | ~629,342 | ~7,068 |

Notes:

- `ScanWarm` (50 keys, all cached, zero fetches asserted): the 400 ms
  global gate floor is gone, so the number is now pure walk + disk-cache
  cost under `-concurrency 3`.
- `ScanColdFakeProvider`'s loopback host is now UNGATED, as designed:
  its host (`127.0.0.1`) is unknown to the per-host limiter (unknown
  host -> no gate), its test source does a raw `client.Do` outside
  `metadata`'s HTTP layer, and `Scan` passes `gateInterval 0` - so the
  number reflects real fetch cost (walk + 20 loopback round trips under
  `-concurrency 3`), not the old `19*400 ms` gate floor.
- `FetchAll1000`/`CacheHit`/`BuildPlan7847` exercise code paths this
  change does not touch; their deltas are run-to-run noise on this box.

## Index second-pass - 2026-10-09 - `go1.27.0 windows/amd64` (AMD Ryzen 7 5800X)

`BenchmarkScanIndexSecondPass` (`bench_test.go`): warm re-scan of a
synthetic 50-file corpus (50 unique shows, one file per folder) with a
populated folder index (second pass) versus without the index. Both legs
use `b.TempDir()` caches and the in-process `fakeSource` (no network);
the `NoIndex` leg prefills and times plain `Scan` (second pass: full walk
+ disk-cache hits), the `WithIndex` leg prefills with `ScanWithOptions`
(`UseIndex`, which writes the index) and times the indexed second pass
(all subtrees skipped). Both legs assert zero fetches during measurement.

Exact command (from `go/`):

```powershell
go test -run '^$' -bench BenchmarkScanIndexSecondPass -benchmem -count=3 ./internal/library/
```

Mean of `-count=3` (per-op; `NoIndex` N=60-64, `WithIndex` N=99-100 per count):

| Benchmark | ns/op | B/op | allocs/op | Notes |
|---|---|---|---|---|
| `BenchmarkScanIndexSecondPass/NoIndex` | ~17,100,771 | ~460,492 | ~9,255 | warm re-scan, no index: full walk + 50 disk-cache hits |
| `BenchmarkScanIndexSecondPass/WithIndex` | ~11,693,746 | ~207,159 | ~996 | warm re-scan, populated index: second pass skips every subtree |

Raw output:

```text
BenchmarkScanIndexSecondPass/NoIndex-16         	      63	  17335424 ns/op	  463723 B/op	    9260 allocs/op
BenchmarkScanIndexSecondPass/NoIndex-16         	      60	  17308087 ns/op	  452808 B/op	    9249 allocs/op
BenchmarkScanIndexSecondPass/NoIndex-16         	      64	  16658802 ns/op	  464945 B/op	    9256 allocs/op
BenchmarkScanIndexSecondPass/WithIndex-16       	     100	  11705168 ns/op	  201576 B/op	     997 allocs/op
BenchmarkScanIndexSecondPass/WithIndex-16       	      99	  11970309 ns/op	  208875 B/op	    1006 allocs/op
BenchmarkScanIndexSecondPass/WithIndex-16       	     100	  11405761 ns/op	  211026 B/op	     986 allocs/op
```

Notes:

- Indexed second pass is **~1.5x faster** (17.1 ms -> 11.7 ms): the
  walk, parse, and resolve of all 50 files is skipped. The remaining
  cost is the tier-1 skip guard, which still verifies every recorded
  cache key against the metadata cache (one disk stat per key), plus
  the index load/save - so the win on a small corpus is modest; it
  grows with file count per skipped folder.
- Allocs drop ~9x (9,255 -> 996): no walked-file plans, groups, or
  resolver results are built for skipped subtrees.

**Superseded (2026-10-13):** the table above is the JSON-index baseline,
kept as the dated measurement it is. The index now lives in `library.idx`
(store-backed; next section), so the first Notes bullet's remaining-cost
claim is no longer true: the indexed scan's per-key guard is always-true
(`scan.go` `keyPresent`) and metadata liveness is the store's own per-file
state, so `BenchmarkScanIndexSecondPass/WithIndex`'s second pass skips
every subtree without touching the metadata cache.

## Store-backed index (`library.idx`) - 2026-10-13 - `go1.27.0 windows/amd64` (AMD Ryzen 7 5800X)

The JSON index (`scanindex.json`, 565,380 bytes, rewritten in full on
every dirty checkpoint) is replaced by one `internal/store` file beside
it - `library.idx`: one record per video file plus a rollup per folder -
and the metadata cache by one `meta.idx` store in place of 1,118 loose
per-key JSON files. Frames are `tag | len | crc32 | payload` behind a
24-byte CRC'd header; replay truncates a torn tail instead of erroring;
a commit fsyncs. The controlled payload codec (binary varints) measures
**~218 B per file record against ~429 B of JSON** for a 92-character
path (`TestFileRecordEncodingBudget`).

Store micro-bench, from `go/`:

```powershell
go test ./internal/store -run '^$' -bench . -benchtime=10x -v
```

| Measurement | Result |
|---|---|
| `BenchmarkPut23402`: 23,402 puts + commit (fsync) | 38.43 ms total |
| `BenchmarkLoad23402`: open of 24,520 records | 7.386 ms |

Full-size store load (test-logged measurements, not asserted gates):

| Measurement | Result |
|---|---|
| migrated real index (928 folder rollups) | 9.04 ms (`TestMigrationParityRealIndex`) |
| 23,402-record store, 7,556,531 bytes: reopen + decode | 28.6 ms (`TestIndexStoreLoadTime`) |

Scan cost over the owner-shaped corpus (93 folders / 2,300 files / 230
keys, in-process provider, zero network): `WarmDiff` moves every folder's
mtime so each file is re-enumerated and (size, mtime)-diffed, `WarmSkip`
is the steady state with the rollups matching. Exact command (from `go/`):

```powershell
go test ./internal/library -run '^$' -bench BenchmarkScanRealShape -benchtime=1x -benchmem -count=3
```

| Leg | per-op (three runs) | Asserted during measurement |
|---|---|---|
| `BenchmarkScanRealShape/WarmDiff` | 141.9 / 143.6 / 149.7 ms | `Parsed==0`, `fetches==0` |
| `BenchmarkScanRealShape/WarmSkip` | 6.4 / 7.6 / 7.7 ms | `Files==0`, `FilesSkipped==2,300`, `fetches==0` |

Notes:

- The full-scale leg (`POTFLIX_BENCH_REALSHAPE=full`: 930 folders /
  23,000 files / 2,300 keys) was **NOT re-run** for this change - it
  needs a multi-minute seeding pass and never converges under
  `-benchtime` on this box. The reduced default above (93 folders /
  2,300 files / 230 keys, from `realShapeScale`) is the benched corpus.
- `Scan` performs zero fetches by construction: the walk enumerates and
  diffs, and metadata resolution is the background catch-up
  (`catchup.go`, started at launch), bounded at 3 concurrent lookups.
- Migration on first open: 928 folders / 23,402 files imported from the
  legacy index (renamed `scanindex.json.bak`, never deleted) and 1,118
  entries imported from the legacy `meta\` directory (kept in place as
  the recovery copy; only `metadata.ClearCache` removes it).
- The metadata cache is one store file behind one held handle per cache
  directory, so a read is a map lookup instead of a whole-file replay.
  That removes the old 64 KiB per-entry read cap, under which four
  entries on this machine (96 KB - 232 KB) were permanently treated as
  "not cached". No per-operation cache timing is quoted here.

// Benchmark harness for the metadata hot path (measurement only).
//
// These benchmarks change no production behaviour: the corpus is fixed,
// the cache lives under b.TempDir() (never %LOCALAPPDATA%\Potflix\meta),
// and no benchmark can reach the real network (empty/offline sources or
// in-process fakes only). Baseline table: ../library/BENCH.md.
package metadata

import (
	"context"
	"testing"
	"time"

	"potflix/internal/titleclean"
)

// benchItem pairs a player-reported basename with its full playing-file path
// so Clean/ResolveQuery/ContextFromPath all exercise realistic input.
type benchItem struct {
	raw  string
	path string
}

// benchCorpus is a fixed ~30-entry corpus of real library patterns:
// SxxEyy series, absolute-numbered anime, movies, folder-context bare
// numbers, scene tags (PROPER/REPACK/INTERNAL), themes and extras.
var benchCorpus = []benchItem{
	{`Show.S01E02.1080p.WEB-DL.x264-GRP.mkv`, `D:\Media\TV\Show\Season 1\Show.S01E02.1080p.WEB-DL.x264-GRP.mkv`},
	{`Breaking.Bad.S01E01.1080p.WEB-DL.DD5.1.x264-GRP.mkv`, `D:\Media\TV\Breaking Bad\Season 1\Breaking.Bad.S01E01.1080p.WEB-DL.DD5.1.x264-GRP.mkv`},
	{`Mr.Robot.S02E05.720p.HDTV.x264-KILLERS.mkv`, `D:\Media\TV\Mr Robot\Season 2\Mr.Robot.S02E05.720p.HDTV.x264-KILLERS.mkv`},
	{`The.Office.US.S05E14.1080p.BluRay.x264-DEMAND.mkv`, `D:\Media\TV\The Office\Season 5\The.Office.US.S05E14.1080p.BluRay.x264-DEMAND.mkv`},
	{`[SubsPlease] Re Zero - 066 [1080p].mkv`, `E:\Anime\Re Zero\[SubsPlease] Re Zero - 066 [1080p].mkv`},
	{`[GRP] Attack on Titan - 059 [1080p].mkv`, `E:\Anime\Attack on Titan\[GRP] Attack on Titan - 059 [1080p].mkv`},
	{`[HorribleSubs] Naruto Shippuuden - 321 [720p].mkv`, `E:\Anime\Naruto Shippuuden\[HorribleSubs] Naruto Shippuuden - 321 [720p].mkv`},
	{`One Piece - 1001 [1080p].mkv`, `E:\Anime\One Piece\One Piece - 1001 [1080p].mkv`},
	{`[GRP] Show Name - 001v2 [1080p].mkv`, `E:\Anime\Show Name\[GRP] Show Name - 001v2 [1080p].mkv`},
	{`Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-GRP.mkv`, `D:\Media\Movies\Dune Part Two (2024)\Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-GRP.mkv`},
	{`Title (2019) 2160p.BluRay.x265.mkv`, `D:\Media\Movies\Title (2019)\Title (2019) 2160p.BluRay.x265.mkv`},
	{`Blade.Runner.2049.2017.IMAX.2160p.BluRay.x265-TERMINAL.mkv`, `D:\Media\Movies\Blade Runner 2049 (2017)\Blade.Runner.2049.2017.IMAX.2160p.BluRay.x265-TERMINAL.mkv`},
	{`Film (1999) 720p.BluRay.x264-GRP.mkv`, `D:\Media\Movies\Film (1999)\Film (1999) 720p.BluRay.x264-GRP.mkv`},
	{`Movie Title 2 (2021) 1080p BluRay x264.mkv`, `D:\Media\Movies\Movie Title 2 (2021)\Movie Title 2 (2021) 1080p BluRay x264.mkv`},
	{`01.mkv`, `D:\Media\TV\Naruto\Season 1\01.mkv`},
	{`12.mkv`, `E:\Anime\One Piece\12.mkv`},
	{`S02E03.mkv`, `D:\Media\TV\Mandalorian\Season 2\S02E03.mkv`},
	{`Show.S03E11.PROPER.1080p.WEB-DL.DD5.1.H.264-GRP.mkv`, `D:\Media\TV\Show\Season 3\Show.S03E11.PROPER.1080p.WEB-DL.DD5.1.H.264-GRP.mkv`},
	{`Show.S03E12.REPACK.720p.HDTV.x264-GRP.mkv`, `D:\Media\TV\Show\Season 3\Show.S03E12.REPACK.720p.HDTV.x264-GRP.mkv`},
	{`Show.S01E01.INTERNAL.1080p.WEB.H264-GRP.mkv`, `D:\Media\TV\Show\Season 1\Show.S01E01.INTERNAL.1080p.WEB.H264-GRP.mkv`},
	{`Show.S04E07.2160p.DSNP.WEB-DL.DDP5.1.Atmos.HDR.H.265-GRP.mkv`, `D:\Media\TV\Show\Season 4\Show.S04E07.2160p.DSNP.WEB-DL.DDP5.1.Atmos.HDR.H.265-GRP.mkv`},
	{`Show Name S01 E04 1080p WEB-DL.mkv`, `D:\Media\TV\Show Name\Season 1\Show Name S01 E04 1080p WEB-DL.mkv`},
	{`Show.Name.E15.1080p.WEB-DL.x264.mkv`, `E:\Anime\Show Name\Show.Name.E15.1080p.WEB-DL.x264.mkv`},
	{`Show.Name.Episode.53+54.1080p.mkv`, `E:\Anime\Show Name\Show.Name.Episode.53+54.1080p.mkv`},
	{`Show.S02E12-E13.1080p.HDTV.x264.mkv`, `D:\Media\TV\Show\Season 2\Show.S02E12-E13.1080p.HDTV.x264.mkv`},
	{`Anime Title - OP 1.mkv`, `E:\Anime\Anime Title\Anime Title - OP 1.mkv`},
	{`SomeShow trailer.mkv`, `D:\Media\TV\SomeShow\Extras\SomeShow trailer.mkv`},
	{`featurette.mkv`, `D:\Media\Movies\Film (1999)\Deleted Scenes\featurette.mkv`},
	{`Show.S01E01.mkv`, `D:\Media\TV\Mr.Robot.S01.1080p.BluRay-GRP\Show.S01E01.mkv`},
	{`Documentary.2020.1080p.WEB-DL.x264-GRP.mkv`, `D:\Media\TV\Documentary\Documentary.2020.1080p.WEB-DL.x264-GRP.mkv`},
	{`Breaking.Bad.S01E01.mkv - PotPlayer`, `D:\Media\TV\Breaking Bad\Season 1\Breaking.Bad.S01E01.mkv - PotPlayer`},
}

var (
	benchCleanSink   titleclean.Cleaned
	benchQuerySink   Query
	benchShowSink    string
	benchSeasonSink  int
	benchResolveSink Resolution
)

// BenchmarkCleanTitle measures raw-filename cleaning over the corpus.
func BenchmarkCleanTitle(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchCleanSink = titleclean.Clean(benchCorpus[i%len(benchCorpus)].raw)
	}
}

// BenchmarkResolveQuery measures filename+folder-context parsing over the corpus.
func BenchmarkResolveQuery(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e := benchCorpus[i%len(benchCorpus)]
		benchQuerySink = ResolveQuery(e.raw, e.path)
	}
}

// BenchmarkContextFromPath measures folder-context derivation over the corpus.
func BenchmarkContextFromPath(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchShowSink, benchSeasonSink = ContextFromPath(benchCorpus[i%len(benchCorpus)].path)
	}
}

// BenchmarkCacheHit measures the full cache-hit path: one entry is pre-stored
// in a b.TempDir() cache, then LookupNowForceSrc(force=false) is timed. The
// resolver carries an empty source list AND Offline:true, so even a miss could
// never touch the network. Steady state is a memory hit after the first op
// warms it from disk.
func BenchmarkCacheHit(b *testing.B) {
	dir := b.TempDir()
	q := Query{Kind: KindSeries, Title: "Breaking Bad", Year: 2008, Season: 1, Episode: 1}
	media := &Media{
		Source: "tvmaze", SourceID: "169",
		Kind: KindSeries, Title: "Breaking Bad", Year: 2008,
		Episodes:  []Episode{{Season: 1, Number: 1, Title: "Pilot"}},
		FetchedAt: time.Now().UTC(),
	}
	if err := cacheStore(dir, cacheKey(q), media); err != nil {
		b.Fatalf("cacheStore: %v", err)
	}
	r := New(Options{Dir: dir, Sources: []Source{}, Offline: true})
	ctx := context.Background()
	if res, fromCache, err := r.LookupNowForceSrc(ctx, q, false); err != nil || !res.Found || !fromCache {
		b.Fatalf("warmup hit = %+v,%v,%v; want Found+fromCache", res, fromCache, err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, fromCache, err := r.LookupNowForceSrc(ctx, q, false)
		if err != nil || !res.Found || !fromCache {
			b.Fatalf("hit = %+v,%v,%v; want Found+fromCache", res, fromCache, err)
		}
		benchResolveSink = res
	}
}

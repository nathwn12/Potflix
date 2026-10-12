package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The benches use the shapes the store replaces: the library index (23,402
// file-sized records, ~150 B of key+value) and the metadata cache (1,118
// one-JSON-per-key entries, ~2.7 KiB each).

const (
	benchIndexRecords = 23402
	benchMetaRecords  = 1118
)

type benchRecord struct {
	key string
	val []byte
}

func indexRecords() []benchRecord {
	out := make([]benchRecord, benchIndexRecords)
	for i := range out {
		// (season, episode) is unique for every i, so every key differs.
		season, episode := i/500+1, i%500
		out[i] = benchRecord{
			key: fmt.Sprintf(`Q:\Media\Shows\Show %02d\Season %02d\Show %02d - S%02dE%03d - 1080p.mkv`,
				i%40, season, i%40, season, episode),
			val: []byte(fmt.Sprintf(`{"title":"Show %02d - S%02dE%03d","season":%d,"episode":%d,"resolved":true,"score":%d}`,
				i%40, season, episode, season, episode, i%100)),
		}
	}
	return out
}

func metaRecords() []benchRecord {
	out := make([]benchRecord, benchMetaRecords)
	body := strings.Repeat("lorem ipsum dolor sit amet ", 99)
	for i := range out {
		out[i] = benchRecord{
			key: fmt.Sprintf("series|show %03d|2024|%d|%d", i%97, i%7+1, i%24),
			val: []byte(fmt.Sprintf(`{"schema":3,"title":"Show %03d","synopsis":"%s","art":"https://example.invalid/a/%06d.jpg"}`,
				i%97, body, i)),
		}
	}
	return out
}

func meanRecordBytes(recs []benchRecord) int {
	total := 0
	for _, r := range recs {
		total += len(r.key) + len(r.val)
	}
	return total / len(recs)
}

// BenchmarkPut23402 writes the 23,402-record index corpus into a fresh file
// (default options, so the delta budget also compacts) and fsyncs it.
//
//	go test ./internal/store -run '^$' -bench BenchmarkPut23402 -benchtime=10x
func BenchmarkPut23402(b *testing.B) {
	recs := indexRecords()
	var total time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		path := filepath.Join(b.TempDir(), "index.pflx")
		b.StartTimer()

		db, err := Open(path, Options{})
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		start := time.Now()
		for _, r := range recs {
			if err := db.Put(r.key, r.val); err != nil {
				b.Fatalf("Put: %v", err)
			}
		}
		if err := db.Commit(); err != nil {
			b.Fatalf("Commit: %v", err)
		}
		write := time.Since(start)
		if err := db.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		total += write
	}
	b.StopTimer()
	meanMS := float64(total.Microseconds()) / float64(b.N) / 1000
	b.ReportMetric(meanMS, "ms/write")
	b.Logf("corpus: %d records, mean %d B of key+value; write+fsync %.1f ms",
		len(recs), meanRecordBytes(recs), meanMS)
}

// BenchmarkLoad23402 measures Open over a file holding the index corpus and
// the metadata corpus - the snapshot read plus the delta replay.
//
//	go test ./internal/store -run '^$' -bench BenchmarkLoad23402 -benchtime=10x
func BenchmarkLoad23402(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "index.pflx"), Options{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	idx, meta := indexRecords(), metaRecords()
	for _, r := range idx {
		if err := db.Put(r.key, r.val); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
	for _, r := range meta {
		if err := db.Put(r.key, r.val); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}
	if err := db.Commit(); err != nil {
		b.Fatalf("Commit: %v", err)
	}
	if err := db.Close(); err != nil {
		b.Fatalf("Close: %v", err)
	}
	b.Logf("corpus: %d index records (mean %d B) + %d meta records (mean %d B)",
		len(idx), meanRecordBytes(idx), len(meta), meanRecordBytes(meta))

	var total, first time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		opened, err := Open(db.path, Options{})
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		load := time.Since(start)
		got := opened.Len()
		if err := opened.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		if want := len(idx) + len(meta); got != want {
			b.Fatalf("Len = %d, want %d", got, want)
		}
		if i == 0 {
			first = load
		}
		total += load
	}
	b.StopTimer()
	meanMS := float64(total.Microseconds()) / float64(b.N) / 1000
	b.ReportMetric(meanMS, "ms/load")
	b.Logf("load of %d records: %.1f ms first run, %.1f ms mean of %d runs",
		len(idx)+len(meta), first.Seconds()*1000, meanMS, b.N)
}

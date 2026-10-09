// Command metacheck is the proof/diagnostic CLI for the metadata lookup:
//
//	metacheck [-offline] [-dir <cacheDir>] <rawTitle>
//
// It parses the raw PotPlayer window title, resolves the canonical series and
// episode title, and prints one line:
//
//	input=<raw> series=<canonical> season=<n> episode=<n> title=<episode> source=<...> cache=<hit|miss> dir=<cacheDir> art=<url> synopsis=<text>
//
// Default mode fetches over the network (LookupNow) and writes the cache.
// With -offline it serves the cache only and never touches the network.
// Exit 0 when resolved, 1 when not found or on error.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"potflix/internal/metadata"
)

// lookupTimeout bounds the online fetch across all providers.
const lookupTimeout = 90 * time.Second

// effectiveDir mirrors the resolver's default cache dir
// (%LOCALAPPDATA%\Potflix\meta, exe-dir fallback) so the printed dir is the
// dir actually used.
func effectiveDir(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return dir
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "Potflix", "meta")
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return filepath.Join(filepath.Dir(exe), "meta")
	}
	return filepath.Join(".", "meta")
}

// cacheSource scans the cache dir for the entry holding res.Series and
// reports its provider name. "" when the entry is absent (a miss).
func cacheSource(dir, series string) string {
	if series == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type media struct {
		Source string `json:"source"`
		Title  string `json:"title"`
	}
	type cacheFile struct {
		Media media `json:"media"`
	}
	fallback := ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var cf cacheFile
		if err := json.Unmarshal(data, &cf); err != nil {
			continue
		}
		if cf.Media.Title == series {
			return cf.Media.Source
		}
		if fallback == "" && strings.EqualFold(cf.Media.Title, series) {
			fallback = cf.Media.Source
		}
	}
	return fallback
}

func main() {
	offline := flag.Bool("offline", false, "cache-only: never touch the network")
	dirFlag := flag.String("dir", "", "cache directory (default %LOCALAPPDATA%\\Potflix\\meta)")
	pathFlag := flag.String("path", "", "full path of the playing file (folder-aware ResolveQuery)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: metacheck [-offline] [-dir <cacheDir>] [-path <filePath>] <rawTitle>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	raw := flag.Arg(0)
	dir := effectiveDir(*dirFlag)
	q := metadata.ParseRaw(raw)
	if strings.TrimSpace(*pathFlag) != "" {
		q = metadata.ResolveQuery(raw, *pathFlag)
	}

	var res metadata.Resolution
	cache := "miss"
	if !*offline {
		// Cache-only probe first: it tells hit from miss without fetching.
		probe := metadata.New(metadata.Options{Dir: dir, Offline: true})
		if pres, err := probe.LookupNow(context.Background(), q); err == nil && pres.Found {
			res, cache = pres, "hit"
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			defer cancel()
			r, err := metadata.New(metadata.Options{Dir: dir}).LookupNow(ctx, q)
			if err != nil {
				fmt.Fprintf(os.Stderr, "metacheck: lookup: %v\n", err)
				printLine(raw, metadata.Resolution{}, "", "miss", dir)
				os.Exit(1)
			}
			res = r
		}
	} else {
		probe := metadata.New(metadata.Options{Dir: dir, Offline: true})
		r, err := probe.LookupNow(context.Background(), q)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metacheck: offline lookup: %v\n", err)
			printLine(raw, metadata.Resolution{}, "", "miss", dir)
			os.Exit(1)
		}
		res = r
		if res.Found {
			cache = "hit"
		}
	}

	source := ""
	if res.Found {
		source = cacheSource(dir, res.Series)
	}
	printLine(raw, res, source, cache, dir)
	if !res.Found {
		os.Exit(1)
	}
}

func printLine(raw string, res metadata.Resolution, source, cache, dir string) {
	buttons := ""
	if len(res.Buttons) > 0 {
		parts := make([]string, 0, len(res.Buttons))
		for _, b := range res.Buttons {
			parts = append(parts, b.Label+"|"+b.URL)
		}
		buttons = strings.Join(parts, ",")
	}
	fmt.Printf("input=%s series=%s season=%d episode=%d title=%s source=%s cache=%s dir=%s genres=%s length=%s year=%d buttons=%s art=%s synopsis=%s\n",
		raw, res.Series, res.Season, res.Number, res.Episode, source, cache, dir, res.Genres, res.Length, res.Year, buttons, res.Artwork, res.Synopsis)
}

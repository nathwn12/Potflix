# Go-written fixtures (cross-implementation gate)

Both files were written by the **Go implementation** (`go\internal\store`) on
2026-10-13, from SYNTHETIC keys only (`k1`..`k5`, values `v1`..`v4-tombstoned`).
No real library or metadata data is in them.

| file | bytes | sha256 (first 16) | shape |
| --- | --- | --- | --- |
| `go-append-delta-v1.pflx` | 139 | `90cf8f92ab1a82b6` | header + put/put/put/put/put/delete/put frames in operation order (overwrite + tombstone) |
| `go-compacted-v1.pflx` | 82 | `a09f4b4f396d08f0` | header + one put frame per live key, in sorted key order (after `DB.Compact`) |

Both hold the same four live records, which is what
`tests::go_written_fixtures_open_identically` asserts:

```
count=4
k1=v1
k2=v2
k3=v3
k5=          <- zero-length value
```

`tests::rust_reproduces_the_go_written_bytes` goes further and asserts the
**byte-for-byte** equality of these files with files this crate writes from the
same operation sequence, so the format is pinned in both directions.

## How they were produced

A throwaway program in the `go` module tree (deleted after the run -- a
`potflix/internal/*` package can only be imported from inside the module):

```go
// go run ./cmd/genstorefixture-<session> ..\build\parity\synthetic-go-store-<session>
package main

import ("fmt"; "os"; "path/filepath"; "sort"; "potflix/internal/store")

func ops(db *store.DB) error {
	put := func(k, v string) error { return db.Put(k, []byte(v)) }
	for _, p := range [][2]string{
		{"k1", "v1"}, {"k2", "v2-old"}, {"k3", "v3"},
		{"k4", "v4-tombstoned"}, {"k5", ""},
	} {
		if err := put(p[0], p[1]); err != nil { return err }
	}
	if err := db.Delete("k4"); err != nil { return err }
	return put("k2", "v2")
}

// ... then Compact() for the second file, Commit(), Close(), and a re-open
// that prints Len() plus the sorted key/value list.
```

Generator output (verbatim):

```
file=go-append-delta-v1.pflx size=139 count=4
  k1=v1
  k2=v2
  k3=v3
  k5=
file=go-compacted-v1.pflx size=82 count=4
  k1=v1
  k2=v2
  k3=v3
  k5=
```

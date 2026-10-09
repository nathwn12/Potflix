package player

import "testing"

// pathquery_test.go - pure-logic tests for DecodeCopyDataString. No live
// PotPlayer is needed.

func TestDecodeCopyDataStringMatch(t *testing.T) {
	got, ok := DecodeCopyDataString(0x6020, []byte("Q:\\Videos\\movie.mkv"))
	if !ok {
		t.Fatal("DecodeCopyDataString(0x6020, path) ok = false, want true")
	}
	if want := "Q:\\Videos\\movie.mkv"; got != want {
		t.Errorf("DecodeCopyDataString(0x6020, path) = %q, want %q", got, want)
	}
}

func TestDecodeCopyDataStringWrongDwData(t *testing.T) {
	if _, ok := DecodeCopyDataString(20486, []byte("Q:\\Videos\\movie.mkv")); ok {
		t.Error("DecodeCopyDataString(20486, path) ok = true, want false")
	}
	if _, ok := DecodeCopyDataString(0, []byte("Q:\\Videos\\movie.mkv")); ok {
		t.Error("DecodeCopyDataString(0, path) ok = true, want false")
	}
}

func TestDecodeCopyDataStringEmpty(t *testing.T) {
	if _, ok := DecodeCopyDataString(0x6020, nil); ok {
		t.Error("DecodeCopyDataString(0x6020, nil) ok = true, want false")
	}
	if _, ok := DecodeCopyDataString(0x6020, []byte{}); ok {
		t.Error("DecodeCopyDataString(0x6020, empty) ok = true, want false")
	}
}

func TestDecodeCopyDataStringTrailingNUL(t *testing.T) {
	got, ok := DecodeCopyDataString(0x6020, []byte("Q:\\Videos\\movie.mkv\x00"))
	if !ok {
		t.Fatal("DecodeCopyDataString(0x6020, NUL-terminated) ok = false, want true")
	}
	if want := "Q:\\Videos\\movie.mkv"; got != want {
		t.Errorf("DecodeCopyDataString(0x6020, NUL-terminated) = %q, want %q", got, want)
	}
}

func TestDecodeCopyDataStringNonNULPreserved(t *testing.T) {
	// No trailing NUL: the bytes pass through untouched, including any
	// embedded NUL.
	in := "Q:\\Vid\x00eos\\movie.mkv"
	got, ok := DecodeCopyDataString(0x6020, []byte(in))
	if !ok {
		t.Fatal("DecodeCopyDataString(0x6020, embedded NUL) ok = false, want true")
	}
	if got != in {
		t.Errorf("DecodeCopyDataString(0x6020, embedded NUL) = %q, want %q", got, in)
	}
	// A single trailing NUL is trimmed; a double trailing NUL keeps one.
	got, ok = DecodeCopyDataString(0x6020, []byte("Q:\\Videos\\movie.mkv\x00\x00"))
	if !ok {
		t.Fatal("DecodeCopyDataString(0x6020, double NUL) ok = false, want true")
	}
	if want := "Q:\\Videos\\movie.mkv\x00"; got != want {
		t.Errorf("DecodeCopyDataString(0x6020, double NUL) = %q, want %q", got, want)
	}
}

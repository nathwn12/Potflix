package metadata

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestDecodeCappedReusesConnection proves the keep-alive fix: N sequential
// fetchGet+decodeCapped round trips from one shared client against the
// same host must reuse ONE TCP connection (baseline: N connections).
// Connections are counted with a ConnState StateNew hook (counting
// accepted sockets, not RemoteAddr strings, which undercount sequential
// connections when the client reuses an ephemeral port).
//
// The server pads its JSON with 64 KB of trailing whitespace (valid JSON:
// Decode succeeds). Padding past the decoder's read buffer is what makes
// the test discriminate: a tiny body is buffered whole by the decoder, so
// even the baseline reuses that connection; with 64 KB outstanding,
// Decode returns while ~63.5 KB is still unread, and only the bounded
// post-decode drain returns the connection to the pool.
func TestDecodeCappedReusesConnection(t *testing.T) {
	const pad = 64 << 10
	body := "{\"ok\":true}" + strings.Repeat(" ", pad)

	mux := http.NewServeMux()
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()
	client := srv.Client()

	ctx := context.Background()
	const n = 5
	for i := 0; i < n; i++ {
		resp, err := fetchGet(ctx, client, srv.URL+"/meta")
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		var v struct {
			OK bool `json:"ok"`
		}
		if err := decodeCapped(resp, &v); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if !v.OK {
			t.Fatalf("decode %d: ok=false, want true", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("server connections = %d, want 1 (%d sequential fetches must reuse one connection)", conns, n)
	}
}

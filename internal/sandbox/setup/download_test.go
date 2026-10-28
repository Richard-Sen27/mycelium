package setup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHTTPDownloader runs against a local TLS server only.
func TestHTTPDownloader(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			_, _ = w.Write(body)
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/ok", http.StatusFound)
	})
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if n == 0 {
			_, _ = w.Write(body)
			return
		}
		http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
	})
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2000")
		_, _ = w.Write(body)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	d := HTTPDownloader{Client: srv.Client(), IdleTimeout: 200 * time.Millisecond}
	ctx := context.Background()

	var out bytes.Buffer
	if err := d.Get(ctx, srv.URL+"/ok", 1000, &out); err != nil || out.Len() != 1000 {
		t.Fatalf("ok: %v, %d bytes", err, out.Len())
	}
	out.Reset()
	if err := d.Get(ctx, srv.URL+"/hop/5", 2000, &out); err != nil || out.Len() != 1000 {
		t.Fatalf("five redirects: %v", err)
	}
	for _, tc := range []struct {
		path string
		max  int64
		want string
		is   error
	}{
		{"/ok", 999, "", ErrTooLarge},       // Content-Length check
		{"/chunked", 3000, "", ErrTooLarge}, // body limit without Content-Length
		{"/to-http", 2000, "non-https", nil},
		{"/hop/6", 2000, "more than 5 redirects", nil},
		{"/stall", 4000, "", ErrStalled},
	} {
		out.Reset()
		err := d.Get(ctx, srv.URL+tc.path, tc.max, &out)
		if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", tc.path, err)
		}
	}
	if err := d.Get(ctx, "http://example.test/x", 10, &out); err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Errorf("plain http: %v", err)
	}
}

func TestMaxBytes(t *testing.T) {
	if got := maxBytes(Source{SizeMB: 101}); got != 202<<20 {
		t.Fatal(got)
	}
	if got := maxBytes(Source{}); got != 2<<20 {
		t.Fatal(got)
	}
}

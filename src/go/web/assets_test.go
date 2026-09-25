package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testAssets() http.FileSystem {
	return http.FS(fstest.MapFS{
		"assets/index-abc123.js": {Data: []byte(strings.Repeat("console.log('phenix');\n", 500))},
		"assets/logo-abc123.png": {Data: []byte("\x89PNG not really")},
	})
}

func get(t *testing.T, h http.Handler, target, acceptEncoding string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec.Result()
}

func TestStaticHandlerGzipsText(t *testing.T) {
	h := StaticHandler(testAssets(), true)
	want := strings.Repeat("console.log('phenix');\n", 500)

	// twice: the second response comes from the compressed cache
	for range 2 {
		resp := get(t, h, "/assets/index-abc123.js", "gzip, deflate, br")

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}

		if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}

		if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "javascript") {
			t.Errorf("Content-Type = %q, want javascript", got)
		}

		if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want Accept-Encoding", got)
		}

		if got := resp.Header.Get("Cache-Control"); got != immutableCacheControl {
			t.Errorf("Cache-Control = %q, want %q", got, immutableCacheControl)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if len(body) >= len(want) {
			t.Errorf("compressed body is %d bytes, not smaller than %d", len(body), len(want))
		}

		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		plain, _ := io.ReadAll(zr)
		if string(plain) != want {
			t.Error("decompressed body does not match the file")
		}
	}
}

func TestStaticHandlerPlain(t *testing.T) {
	h := StaticHandler(testAssets(), false)

	cases := map[string]struct{ target, acceptEncoding string }{
		"no Accept-Encoding": {"/assets/index-abc123.js", ""},
		"gzip refused":       {"/assets/index-abc123.js", "gzip;q=0, br"},
		"binary file":        {"/assets/logo-abc123.png", "gzip"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := get(t, h, c.target, c.acceptEncoding)
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}

			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q, want none", got)
			}

			if got := resp.Header.Get("Cache-Control"); got != "" {
				t.Errorf("Cache-Control = %q, want none for non-hashed assets", got)
			}
		})
	}
}

func TestStaticHandlerMissing(t *testing.T) {
	resp := get(t, StaticHandler(testAssets(), true), "/assets/nope.js", "gzip")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	// a missing asset may be deployed later, so the 404 must not be cached
	if got := resp.Header.Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q, want none on 404", got)
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                 false,
		"br":               false,
		"gzip":             true,
		"GZIP":             true,
		"deflate, gzip":    true,
		"gzip;q=0.5":       true,
		"gzip;q=0":         false,
		"gzip; q=0.000":    false,
		"x-gzip, identity": false,
	}

	for header, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", header)

		if got := acceptsGzip(req); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

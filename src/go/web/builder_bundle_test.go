package web

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuilderBundleHandler(t *testing.T) {
	grapheditor, err := fs.Sub(publicFS, "public/grapheditor")
	if err != nil {
		t.Fatal(err)
	}

	handler := BuilderBundleHandler(grapheditor, false)

	req := httptest.NewRequest(http.MethodGet, "/grapheditor/builder.bundle.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, encoding %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "// --- src/js/view/mxGraph.js\n") {
		t.Fatal("bundle is missing mxGraph")
	}

	// a cached copy is revalidated without resending the bundle
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation status %d, want %d", rec.Code, http.StatusNotModified)
	}
}

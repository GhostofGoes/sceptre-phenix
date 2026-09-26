package forward

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
)

func tunnelerRouter(t *testing.T) *mux.Router {
	t.Helper()

	dir := t.TempDir()
	tunnelerDir = dir

	if err := os.WriteFile(filepath.Join(dir, "phenix-tunneler-linux-amd64"), []byte("bin"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	router := mux.NewRouter()
	router.HandleFunc("/downloads/tunneler", ListTunnelers)
	router.HandleFunc("/downloads/tunneler/{name}", GetTunneler)

	return router
}

func serve(router http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec
}

//nolint:paralleltest // tunnelerDir is shared package state
func TestTunnelerDownloads(t *testing.T) {
	router := tunnelerRouter(t)

	rec := serve(router, "/downloads/tunneler/phenix-tunneler-linux-amd64")
	if rec.Code != http.StatusOK || rec.Body.String() != "bin" {
		t.Fatalf("download: got %d %q", rec.Code, rec.Body.String())
	}

	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=phenix-tunneler-linux-amd64` {
		t.Errorf("Content-Disposition = %q", got)
	}

	if rec := serve(router, "/downloads/tunneler/missing"); rec.Code != http.StatusNotFound {
		t.Errorf("missing build: got %d, want 404", rec.Code)
	}

	if rec := serve(router, "/downloads/tunneler/subdir"); rec.Code != http.StatusNotFound {
		t.Errorf("directory: got %d, want 404", rec.Code)
	}

	rec = serve(router, "/downloads/tunneler")

	var list struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	if len(list.Files) != 1 || list.Files[0] != "phenix-tunneler-linux-amd64" {
		t.Errorf("list = %v", list.Files)
	}
}

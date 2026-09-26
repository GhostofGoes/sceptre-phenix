package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"phenix/util/plog"
	"phenix/web/builderbundle"
)

type builderBundle struct {
	etag    string
	raw, gz []byte
}

// BuilderBundleHandler serves the topology builder's scripts as one file (see
// builderbundle). The bundle is built once, in the background at startup,
// except when rebuild is set (unbundled assets): then it is built per request
// so edits to the grapheditor show up.
func BuilderBundleHandler(grapheditor fs.FS, rebuild bool) http.Handler {
	var (
		mu     sync.Mutex
		bundle *builderBundle
	)

	load := func() (builderBundle, error) {
		mu.Lock()
		defer mu.Unlock()

		if bundle != nil && !rebuild {
			return *bundle, nil
		}

		src, err := builderbundle.Build(grapheditor)
		if err != nil {
			return builderBundle{}, err
		}

		gz, err := gzipBytes(src)
		if err != nil {
			return builderBundle{}, err
		}

		sum := sha256.Sum256(src)
		bundle = &builderBundle{etag: hex.EncodeToString(sum[:16]), raw: src, gz: gz}

		return *bundle, nil
	}

	if !rebuild {
		// build ahead of the first visit, which would otherwise wait on it
		go func() { _, _ = load() }()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asset, err := load()
		if err != nil {
			plog.Error(plog.TypeSystem, "building the topology builder bundle", "err", err)
			http.Error(w, "building the topology builder scripts failed", http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Add("Vary", "Accept-Encoding")

		body := asset.raw
		etag := asset.etag

		if acceptsGzip(r) {
			body = asset.gz
			etag += "-gz"

			w.Header().Set("Content-Encoding", "gzip")
		}

		w.Header().Set("ETag", `"`+etag+`"`)
		http.ServeContent(w, r, "builder.bundle.js", time.Time{}, bytes.NewReader(body))
	})
}

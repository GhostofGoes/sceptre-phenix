package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// the all: prefix is required: without it embed skips files starting with
// '_' or '.', and Vite emits shared chunks named like _baseIsEqual-<hash>.js
//
//go:embed all:public
var publicFS embed.FS

func GetAssets() (http.FileSystem, error) {
	subFS, err := fs.Sub(publicFS, "public")
	if err != nil {
		return nil, err
	}
	return http.FS(subFS), nil
}

// Vite content-hashes every file it writes under assets/, so a given URL never
// changes and browsers need not revalidate it.
const immutableCacheControl = "public, max-age=31536000, immutable"

var compressibleExts = map[string]bool{ //nolint:gochecknoglobals // lookup table
	".css":  true,
	".html": true,
	".js":   true,
	".json": true,
	".map":  true,
	".mjs":  true,
	".svg":  true,
	".txt":  true,
	".xml":  true,
}

type gzipKey struct {
	name    string
	size    int64
	modTime time.Time
}

// StaticHandler serves files from assets, gzip-compressing text assets for
// clients that accept it (the UI's JavaScript compresses roughly 4x). The
// compressed bytes are cached per file; the size and modification time in the
// key keep the cache correct when serving unbundled assets from disk.
func StaticHandler(assets http.FileSystem, immutable bool) http.Handler {
	var (
		files = http.FileServer(assets)
		cache sync.Map // gzipKey -> []byte
	)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if immutable {
			w.Header().Set("Cache-Control", immutableCacheControl)
		}

		name := path.Clean("/" + r.URL.Path)
		if !compressibleExts[path.Ext(name)] {
			files.ServeHTTP(w, r)
			return
		}

		w.Header().Add("Vary", "Accept-Encoding")

		if !acceptsGzip(r) {
			files.ServeHTTP(w, r)
			return
		}

		f, err := assets.Open(name)
		if err != nil {
			files.ServeHTTP(w, r)
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			files.ServeHTTP(w, r)
			return
		}

		key := gzipKey{name: name, size: fi.Size(), modTime: fi.ModTime()}

		var gz []byte
		if cached, ok := cache.Load(key); ok {
			gz, _ = cached.([]byte)
		} else {
			gz, err = gzipFile(f)
			if err != nil {
				files.ServeHTTP(w, r)
				return
			}

			cache.Store(key, gz)
		}

		w.Header().Set("Content-Encoding", "gzip")
		http.ServeContent(w, r, name, fi.ModTime(), bytes.NewReader(gz))
	})
}

func acceptsGzip(r *http.Request) bool {
	for enc := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}

		// "gzip;q=0" means the client refuses gzip
		for param := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(k, "q") {
				q, err := strconv.ParseFloat(v, 64)
				return err == nil && q > 0
			}
		}

		return true
	}

	return false
}

func gzipFile(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(zw, r); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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

type assetKey struct {
	name    string
	size    int64
	modTime time.Time
}

type cachedAsset struct {
	etag string
	gz   []byte // nil unless the file is compressible
}

// StaticHandler serves files from assets, gzip-compressing text assets for
// clients that accept it (the UI's JavaScript compresses roughly 4x). Every
// file gets a content-hash ETag so browsers can revalidate with a 304: embedded
// files have a zero modification time, so there is no Last-Modified to fall
// back on. The ETag and compressed bytes are cached per file; the size and
// modification time in the key keep the cache correct when serving unbundled
// assets from disk.
func StaticHandler(assets http.FileSystem, immutable bool) http.Handler {
	var (
		files = http.FileServer(assets)
		cache sync.Map // assetKey -> cachedAsset
	)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean("/" + r.URL.Path)
		compressible := compressibleExts[path.Ext(name)]

		if compressible {
			w.Header().Add("Vary", "Accept-Encoding")
		}

		f, err := assets.Open(name)
		if err != nil {
			files.ServeHTTP(w, r)
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			files.ServeHTTP(w, r)
			return
		}

		if fi.IsDir() {
			// FileServer redirects directory URLs lacking the trailing slash and
			// lists directories without an index; anything else is the index
			if !strings.HasSuffix(r.URL.Path, "/") {
				files.ServeHTTP(w, r)
				return
			}

			name = path.Join(name, "index.html")
			compressible = true

			index, err := assets.Open(name)
			if err != nil {
				files.ServeHTTP(w, r)
				return
			}
			defer index.Close()

			if fi, err = index.Stat(); err != nil {
				files.ServeHTTP(w, r)
				return
			}

			f = index
			w.Header().Add("Vary", "Accept-Encoding")
		}

		key := assetKey{name: name, size: fi.Size(), modTime: fi.ModTime()}

		var asset cachedAsset
		if cached, ok := cache.Load(key); ok {
			asset, _ = cached.(cachedAsset)
		} else {
			asset, err = loadAsset(f, compressible)
			if err != nil {
				files.ServeHTTP(w, r)
				return
			}

			cache.Store(key, asset)
		}

		// only here, once the file is known to exist: a cached 404 or redirect
		// would outlive the deploy that adds the file
		if immutable {
			w.Header().Set("Cache-Control", immutableCacheControl)
		}

		if asset.gz == nil || !acceptsGzip(r) {
			w.Header().Set("ETag", `"`+asset.etag+`"`)
			files.ServeHTTP(w, r)

			return
		}

		w.Header().Set("ETag", `"`+asset.etag+`-gz"`)
		w.Header().Set("Content-Encoding", "gzip")
		http.ServeContent(w, r, name, fi.ModTime(), bytes.NewReader(asset.gz))
	})
}

func loadAsset(r io.Reader, compressible bool) (cachedAsset, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return cachedAsset{}, err
	}

	sum := sha256.Sum256(raw)
	asset := cachedAsset{etag: hex.EncodeToString(sum[:16]), gz: nil}

	if compressible {
		if asset.gz, err = gzipBytes(raw); err != nil {
			return cachedAsset{}, err
		}
	}

	return asset, nil
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

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}

	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

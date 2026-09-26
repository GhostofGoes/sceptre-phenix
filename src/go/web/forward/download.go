package forward

import (
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/gorilla/mux"

	"phenix/util/plog"
)

// tunnelerDir holds the phenix-tunneler builds offered for download.
var tunnelerDir = "downloads/tunneler" //nolint:gochecknoglobals // overridden in tests

// ListTunnelers - GET /downloads/tunneler.
//
// Lists the builds that are actually installed, so the Tunneler page offers
// only downloads that exist.
func ListTunnelers(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(tunnelerDir)
	if err != nil {
		plog.Error(plog.TypeSystem, "listing tunneler downloads", "err", err)
		http.Error(w, "unable to list tunneler downloads", http.StatusInternalServerError)

		return
	}

	names := []string{}

	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)

	body, _ := json.Marshal(map[string][]string{"files": names})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// GetTunneler - GET /downloads/tunneler/{name}.
func GetTunneler(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]

	// only plain files directly in the downloads directory
	if name == "" || filepath.Base(name) != name || name[0] == '.' {
		http.Error(w, "invalid tunneler name", http.StatusBadRequest)

		return
	}

	file, err := os.Open(filepath.Join(tunnelerDir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "tunneler "+name+" is not installed on this server", http.StatusNotFound)

			return
		}

		plog.Error(plog.TypeSystem, "opening tunneler for download", "name", name, "err", err)
		http.Error(w, "unable to open tunneler "+name, http.StatusInternalServerError)

		return
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "tunneler "+name+" is not installed on this server", http.StatusNotFound)

		return
	}

	w.Header().Set(
		"Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}),
	)
	w.Header().Set("Content-Type", "application/octet-stream")

	http.ServeContent(w, r, name, info.ModTime(), file)
}

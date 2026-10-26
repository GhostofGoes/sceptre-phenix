package web

import (
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gorilla/mux"

	"phenix/util/mm/mmcli"
)

// minimegaRouteVars are the route and query variables that end up in minimega
// commands: experiment and VM names, snapshot names, disk paths, and hosts.
var minimegaRouteVars = []string{ //nolint:gochecknoglobals // constant list
	"exp", "name", "snapshot", "disk", "new", "backing", "size", "host",
}

// rejectUnsafeMinimegaVars refuses requests to experiment, VM, and disk routes
// whose route variables minimega could not read back intact; see
// [mmcli.CheckArg].
func rejectUnsafeMinimegaVars(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")

		if strings.HasPrefix(path, "/experiments/") || strings.HasPrefix(path, "/disks") {
			vars := mux.Vars(r)

			for _, key := range minimegaRouteVars {
				if err := mmcli.CheckArg(vars[key]); err != nil {
					http.Error(w, "invalid "+key+": "+err.Error(), http.StatusBadRequest)

					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// badMinimegaArgs writes a 400 and returns true if any value from a request
// body or query would not reach minimega intact; see [mmcli.CheckArg].
func badMinimegaArgs(w http.ResponseWriter, args ...string) bool {
	if err := mmcli.CheckArgs(args...); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return true
	}

	return false
}

// badMinimegaFilename is badMinimegaArgs for a file name a client chooses for
// a snapshot, capture, or committed disk. The name must also stay relative and
// below the directory phenix puts it in, so it cannot overwrite files elsewhere
// on the host.
func badMinimegaFilename(w http.ResponseWriter, name string) bool {
	if badMinimegaArgs(w, name) {
		return true
	}

	if filepath.IsAbs(name) || slices.Contains(strings.Split(filepath.ToSlash(name), "/"), "..") {
		http.Error(w, "file name must be relative and must not contain ..", http.StatusBadRequest)

		return true
	}

	return false
}

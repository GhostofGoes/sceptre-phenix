package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/util/file"
	"phenix/util/mm"
	"phenix/util/plog"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

const MountPathTimeout = 2 * time.Second

// MountInfo is a small struct for tracking number of users interacting with a mount and the
// lock for that mount.
type MountInfo struct {
	users int
	lock  *sync.RWMutex
}

var (
	// generally, use exclusive WLock for mount/unmount ops; RLock for file interactions.
	activeMounts = make(map[string]*MountInfo) //nolint:gochecknoglobals // global state
	// locks activeMounts map itself.
	activeMountsMu sync.RWMutex //nolint:gochecknoglobals // global lock

	// vmMounted reports whether a VM filesystem is mounted at a host path.
	vmMounted = vm.Mounted //nolint:gochecknoglobals // replaced by tests
)

// lookupMount returns the mount of the VM whose mount path is basePath. A VM
// mounted by another process, such as `phenix vm mount`, is not in
// activeMounts, so it gets a lock of its own: MountVM does not take such a
// mount over, and UnmountVM only unmounts what MountVM mounted.
//
// A mount in activeMounts whose file system is gone, detached by an
// experiment stop or by `phenix vm unmount`, is not mounted.
func lookupMount(basePath string) (*MountInfo, bool) {
	if !vmMounted(basePath) {
		return nil, false
	}

	activeMountsMu.RLock()
	mountInfo, exists := activeMounts[basePath]
	activeMountsMu.RUnlock()

	if exists {
		return mountInfo, true
	}

	return &MountInfo{users: 0, lock: &sync.RWMutex{}}, true
}

// forgetStaleMount drops the entry of activeMounts for mapKey when the VM
// file system it recorded is no longer mounted: an experiment stop and `phenix
// vm unmount` detach mounts without telling this process, and the entry would
// make MountVM answer 200 without mounting. The caller must hold
// activeMountsMu.
func forgetStaleMount(mapKey string) {
	if _, exists := activeMounts[mapKey]; exists && !vmMounted(mapKey) {
		plog.Info(plog.TypeSystem, "forgetting mount that is gone", "mount", mapKey)

		delete(activeMounts, mapKey)
	}
}

// MountVM - POST /experiments/{exp}/vms/{name}/mount.
func MountVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	mapKey := mm.GetLocalMountPath(vars["exp"], vars["name"])

	if !role.Allowed("vms/mount", "post", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Warn(
			plog.TypeSecurity,
			"mounting vm not allowed",
			"user",
			user,
			"mount",
			mapKey,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	activeMountsMu.Lock()
	defer activeMountsMu.Unlock()

	forgetStaleMount(mapKey)

	mountInfo, exists := activeMounts[mapKey]

	switch {
	case exists:
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Info(
			plog.TypeAction,
			"adding additional user to mount",
			"mount",
			mapKey,
			"count",
			mountInfo.users,
			"user",
			user,
		)

		mountInfo.users += 1
	case vmMounted(mapKey):
		// Another process, such as `phenix vm mount`, mounted the VM. Serve its
		// files but leave the mount to that process: recording it here would let
		// the next UnmountVM unmount it.
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Info(plog.TypeAction, "vm already mounted by another process", "mount", mapKey, "user", user)
	default:
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Info(
			plog.TypeAction,
			"vm mounted",
			"exp",
			vars["exp"],
			"vm",
			vars["name"],
			"path",
			mapKey,
			"user",
			user,
		)

		if err := vm.MountFilesystem(vars["exp"], vars["name"]); err != nil {
			plog.Error(plog.TypeSystem, "creating mount", "mount", mapKey, "err", err)
			http.Error(w, fmt.Sprintf("Error mounting: %s", err), vmErrorStatus(err))

			return
		}

		activeMounts[mapKey] = &MountInfo{1, &sync.RWMutex{}}
	}

	w.WriteHeader(http.StatusOK)
}

// UnmountVM - POST /experiments/{exp}/vms/{name}/unmount.
func UnmountVM(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	mapKey := mm.GetLocalMountPath(vars["exp"], vars["name"])

	if !role.Allowed("vms/mount", "delete", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Warn(
			plog.TypeSecurity,
			"unmounting vm not allowed",
			"user",
			user,
			"mount",
			mapKey,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	activeMountsMu.Lock()
	defer activeMountsMu.Unlock()

	// A mount that is gone needs no unmount.
	forgetStaleMount(mapKey)

	mountInfo, exists := activeMounts[mapKey]

	switch {
	case exists:
		if mountInfo.users <= 1 {
			user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
			plog.Info(
				plog.TypeAction,
				"unmounting",
				"mount",
				mapKey,
				"user",
				user,
			)

			mountInfo.lock.Lock()

			// The count stays as it was when the unmount fails, so a retry
			// unmounts again.
			if err := vm.UnmountFilesystem(vars["exp"], vars["name"]); err != nil {
				mountInfo.lock.Unlock()

				plog.Error(plog.TypeSystem, "unmounting", "mount", mapKey, "err", err)
				http.Error(w, fmt.Sprintf("Error unmounting: %s", err), vmErrorStatus(err))

				return
			}

			delete(activeMounts, mapKey)

			// A file handler that looked the mount up before may wait for it.
			mountInfo.lock.Unlock()
		} else {
			mountInfo.users -= 1

			user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
			plog.Info(
				plog.TypeAction,
				"call to unmount but skipping since users remain",
				"mount",
				mapKey,
				"count",
				mountInfo.users,
				"user",
				user,
			)
		}
	case vmMounted(mapKey):
		// The UI unmounts when its file browser closes; the mount of another
		// process stays for that process to unmount.
		plog.Info(plog.TypeAction, "leaving vm mounted by another process", "mount", mapKey)
	default:
		plog.Warn(
			plog.TypeSystem,
			"tried to unmount VM whose lock was not in map",
			"vm",
			vars["name"],
		)
	}

	w.WriteHeader(http.StatusOK)
}

// GetMountFiles - GET /experiments/{exp}/vms/{name}/mount/files?path=
// Note: error may be returned inside json body as Readdir can return an error with entries.
//
//nolint:funlen // handler
func GetMountFiles(w http.ResponseWriter, r *http.Request) {
	var (
		vars     = mux.Vars(r)
		basePath = mm.GetLocalMountPath(vars["exp"], vars["name"])
		role, _  = r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	)

	if !role.Allowed("vms/mount", "list", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Warn(
			plog.TypeSecurity,
			"getting vm mount files not allowed not allowed",
			"user",
			user,
			"mount",
			basePath,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	mountInfo, exists := lookupMount(basePath)
	if !exists {
		http.Error(w, "No existing mount for "+basePath, http.StatusBadRequest)

		return
	}

	mountInfo.lock.RLock()
	defer mountInfo.lock.RUnlock()

	combinedPath := filepath.Join(basePath, vars["path"])

	if err := validatePathWithin(combinedPath, basePath); err != nil {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Error(
			plog.TypeSecurity,
			"user attempted getting path outside of mount",
			"path",
			combinedPath,
			"user",
			user,
		)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: Path is not within mount", combinedPath),
			http.StatusBadRequest,
		)

		return
	}
	user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
	plog.Info(
		plog.TypeAction,
		"getting files from mount",
		"path",
		combinedPath,
		"user",
		user,
	)

	var (
		info fs.FileInfo
		err  error
		done = make(chan struct{})
	)

	go func() {
		info, err = os.Stat(combinedPath)

		close(done)
	}()

	select {
	case <-done:
		if err != nil {
			plog.Error(plog.TypeSystem, "error getting mount path", "path", combinedPath)
			http.Error(
				w,
				fmt.Sprintf("Error getting path %s: %v", combinedPath, err),
				http.StatusInternalServerError,
			)

			return
		}

		if !info.IsDir() {
			http.Error(w, "Expected directory not file: "+combinedPath, http.StatusBadRequest)

			return
		}
	case <-time.After(MountPathTimeout):
		plog.Error(plog.TypeSystem, "timeout getting mount path", "path", combinedPath)
		http.Error(w, "timeout getting path "+combinedPath, http.StatusInternalServerError)

		return
	}

	var dirEntries []fs.DirEntry

	done = make(chan struct{})

	go func() {
		dirEntries, err = os.ReadDir(combinedPath)

		close(done)
	}()

	select {
	case <-done:
		var files file.Files

		for _, e := range dirEntries {
			info, _ := e.Info()
			file := file.MakeFile(info, combinedPath)

			file.Path = strings.TrimPrefix(file.Path, basePath)

			files = append(files, file)
		}

		resp := map[string]any{"error": "", "files": files}

		if err != nil {
			plog.Error(
				plog.TypeSystem,
				fmt.Sprintf(
					"Error getting files in %s. Still read %d entries: %v",
					combinedPath,
					len(dirEntries),
					err,
				),
			)
			resp["error"] = strings.ReplaceAll(fmt.Sprintf("%v", err), basePath, "")
		}

		body, _ := json.Marshal(resp)
		_, _ = w.Write(body)
	case <-time.After(MountPathTimeout):
		err := "timeout getting files in " + combinedPath

		plog.Error(plog.TypeSystem, "timeout getting files", "err", err, "path", combinedPath)
		http.Error(w, err, http.StatusInternalServerError)
	}
}

// DownloadMountFile - GET /experiments/{exp}/vms/{name}/files/download?path=.
func DownloadMountFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	basePath := mm.GetLocalMountPath(vars["exp"], vars["name"])

	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	if !role.Allowed("vms/mount", "get", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Warn(
			plog.TypeSecurity,
			"downloading vm mount files not allowed not allowed",
			"user",
			user,
			"mount",
			basePath,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	mountInfo, exists := lookupMount(basePath)
	if !exists {
		http.Error(w, "No existing mount for "+basePath, http.StatusBadRequest)

		return
	}

	mountInfo.lock.RLock()
	defer mountInfo.lock.RUnlock()

	combinedPath := filepath.Join(basePath, vars["path"])

	if err := validatePathWithin(combinedPath, basePath); err != nil {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Error(
			plog.TypeSecurity,
			"user attempted downloading file outside of mount",
			"path",
			combinedPath,
			"user",
			user,
		)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: Path is not within mount", combinedPath),
			http.StatusBadRequest,
		)

		return
	}

	fileInfo, err := os.Stat(combinedPath)
	if err != nil {
		plog.Error(plog.TypeSystem, "error getting path", "err", err.Error(), "path", combinedPath)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: %s", combinedPath, err.Error()),
			http.StatusInternalServerError,
		)

		return
	}

	if fileInfo.IsDir() {
		http.Error(w, "Can't download directory: "+combinedPath, http.StatusBadRequest)

		return
	}

	user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
	plog.Info(
		plog.TypeAction,
		"download for file",
		"file",
		combinedPath,
		"user",
		user,
	)

	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(fileInfo.Name()))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, combinedPath)
}

// UploadMountFile - PUT /experiments/{exp}/vms/{name}/files/download?path=.
func UploadMountFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	basePath := mm.GetLocalMountPath(vars["exp"], vars["name"])

	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	if !role.Allowed("vms/mount", "patch", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Warn(
			plog.TypeSecurity,
			"uploading vm mount files not allowed not allowed",
			"user",
			user,
			"mount",
			basePath,
		)
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	mountInfo, exists := lookupMount(basePath)
	if !exists {
		http.Error(w, "No existing mount for "+basePath, http.StatusBadRequest)

		return
	}

	mountInfo.lock.RLock()
	defer mountInfo.lock.RUnlock()

	combinedPath := filepath.Join(basePath, vars["path"])

	if err := validatePathWithin(combinedPath, basePath); err != nil {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Error(
			plog.TypeSecurity,
			"user attempted uploading file outside of mount",
			"path",
			combinedPath,
			"user",
			user,
		)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: Path is not within mount", combinedPath),
			http.StatusBadRequest,
		)

		return
	}
	//nolint:gosec // Path traversal via taint analysis
	if _, err := os.Stat(combinedPath); err != nil {
		plog.Error(plog.TypeSystem, "error getting path", "err", err.Error(), "path", combinedPath)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: %s", combinedPath, err.Error()),
			http.StatusInternalServerError,
		)

		return
	}

	clientFile, handler, err := r.FormFile("file")
	if err != nil {
		plog.Error(
			plog.TypeSystem,
			"error uploading file",
			"err",
			err.Error(),
			"dest_path",
			combinedPath,
		)
		http.Error(w, "Error uploading: "+err.Error(), http.StatusBadRequest)

		return
	}

	defer func() { _ = clientFile.Close() }()

	if err := writeUpload(filepath.Join(combinedPath, handler.Filename), clientFile); err != nil {
		plog.Error(plog.TypeSystem, "error uploading file", "err", err.Error(), "dest_path", combinedPath)
		http.Error(w, "Error uploading: "+err.Error(), http.StatusInternalServerError)

		return
	}
}

// writeUpload writes the uploaded file src to path, replacing a file there:
// without truncating it, a shorter upload kept the old file's tail.
func writeUpload(path string, src io.Reader) error {
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path is checked to be within the mount
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()

		return fmt.Errorf("writing %s: %w", path, err)
	}

	if err := dst.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}

	return nil
}

// CopyExperimentFileToMount copies a same-experiment file into a mounted VM directory.
//
//nolint:funlen // complex logic
func CopyExperimentFileToMount(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	basePath := mm.GetLocalMountPath(vars["exp"], vars["name"])

	role, _ := r.Context().Value(middleware.ContextKeyRole).(rbac.Role)
	if !role.Allowed("vms/mount", "patch", fmt.Sprintf("%s/%s", vars["exp"], vars["name"])) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	if !role.Allowed("experiments/files", "get", vars["exp"]) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	mountInfo, exists := lookupMount(basePath)
	if !exists {
		http.Error(w, "No existing mount for "+basePath, http.StatusBadRequest)

		return
	}

	mountInfo.lock.RLock()
	defer mountInfo.lock.RUnlock()

	query := r.URL.Query()
	destDir := filepath.Join(basePath, query.Get("path"))
	if err := validatePathWithin(destDir, basePath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	info, err := os.Stat(destDir)
	if err != nil {
		http.Error(w, "Error getting path "+destDir+": "+err.Error(), http.StatusInternalServerError)

		return
	}

	if !info.IsDir() {
		http.Error(w, "Expected directory not file: "+destDir, http.StatusBadRequest)

		return
	}

	exp, err := experiment.Get(vars["exp"])
	if err != nil {
		http.Error(w, "Error getting experiment: "+err.Error(), http.StatusInternalServerError)

		return
	}

	source, err := getExperimentFilePath(vars["exp"], query.Get("source"), exp.FilesDir())
	if err != nil {
		http.Error(w, "Error getting source file: "+err.Error(), http.StatusBadRequest)

		return
	}

	sourceFile, err := os.Open(source) //nolint:gosec // source validated against experiment file list
	if err != nil {
		http.Error(w, "Error opening source file: "+err.Error(), http.StatusInternalServerError)

		return
	}
	defer func() { _ = sourceFile.Close() }()

	destName := filepath.Base(source)
	destPath := filepath.Join(destDir, destName)
	if err := validatePathWithin(destPath, destDir); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	destFile, err := os.OpenFile(
		destPath,
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0o600,
	) //nolint:gosec // destination validated against mount path
	if err != nil {
		http.Error(w, "Error creating destination file: "+err.Error(), http.StatusInternalServerError)

		return
	}
	defer func() { _ = destFile.Close() }()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		http.Error(w, "Error copying file: "+err.Error(), http.StatusInternalServerError)

		return
	}
}

// getExperimentFilePath returns a source path only when it matches a known experiment file.
func getExperimentFilePath(expName, source, filesDir string) (string, error) {
	if source == "" {
		return "", errors.New("no source file provided")
	}

	files, err := experiment.Files(expName, "")
	if err != nil {
		return "", fmt.Errorf("listing experiment files: %w", err)
	}

	for _, f := range files {
		if source != f.Path {
			continue
		}

		headnode, _ := os.Hostname()
		_ = file.CopyFile(fmt.Sprintf("/%s/files/%s", expName, f.Path), headnode, nil)

		path := filepath.Join(filesDir, f.Path)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}

		if info.IsDir() {
			return "", errors.New("source is a directory")
		}

		return path, nil
	}

	return "", errors.New("file not found")
}

// validatePathWithin returns an error when path escapes base.
func validatePathWithin(path, base string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	if rel == ".." || strings.HasPrefix(rel, "../") {
		return errors.New("path is not within base")
	}

	return nil
}

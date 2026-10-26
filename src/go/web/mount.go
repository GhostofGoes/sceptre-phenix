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
)

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

	mountInfo, exists := activeMounts[mapKey]
	if exists {
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
	} else {
		user, _ := r.Context().Value(middleware.ContextKeyUser).(string)
		plog.Info(
			plog.TypeAction,
			"vm mounted",
			"exp",
			vars["exp"],
			"vm",
			vars["vm"],
			"path",
			mapKey,
			"user",
			user,
		)

		if err := vm.MountFilesystem(vars["exp"], vars["name"]); err != nil {
			plog.Error(plog.TypeSystem, "creating mount", "mount", mapKey, "err", err)
			http.Error(w, fmt.Sprintf("Error mounting: %s", err), http.StatusInternalServerError)

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

	mountInfo, exists := activeMounts[mapKey]
	if exists {
		mountInfo.users -= 1

		if mountInfo.users == 0 {
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

			if err := vm.UnmountFilesystem(vars["exp"], vars["name"]); err != nil {
				mountInfo.lock.Unlock()

				plog.Error(plog.TypeSystem, "unmounting", "mount", mapKey, "err", err)
				http.Error(
					w,
					fmt.Sprintf("Error unmounting: %s", err),
					http.StatusInternalServerError,
				)

				return
			}

			delete(activeMounts, mapKey)
		} else {
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
	} else {
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

	root, release, ok := openActiveMount(w, basePath)
	if !ok {
		return
	}

	defer release()

	var (
		relPath      = mountRelPath(vars["path"])
		combinedPath = filepath.Join(basePath, relPath)
	)

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
		info, err = root.Stat(relPath)

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
		var dir *os.File

		dir, err = root.Open(relPath)
		if err != nil {
			close(done)

			return
		}

		dirEntries, err = dir.ReadDir(-1)
		_ = dir.Close()

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

	root, release, ok := openActiveMount(w, basePath)
	if !ok {
		return
	}

	defer release()

	var (
		relPath      = mountRelPath(vars["path"])
		combinedPath = filepath.Join(basePath, relPath)
	)

	// Opening through the root refuses ".." and symlinks that leave the mount,
	// such as a link the guest file system points at the host's /.
	f, err := root.Open(relPath)
	if err != nil {
		plog.Error(plog.TypeSystem, "error getting path", "err", err.Error(), "path", combinedPath)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: %s", combinedPath, err.Error()),
			http.StatusBadRequest,
		)

		return
	}

	defer func() { _ = f.Close() }()

	fileInfo, err := f.Stat()
	if err != nil {
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
	http.ServeContent(w, r, fileInfo.Name(), fileInfo.ModTime(), f)
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

	root, release, ok := openActiveMount(w, basePath)
	if !ok {
		return
	}

	defer release()

	var (
		relPath      = mountRelPath(vars["path"])
		combinedPath = filepath.Join(basePath, relPath)
	)

	info, err := root.Stat(relPath)
	if err != nil {
		plog.Error(plog.TypeSystem, "error getting path", "err", err.Error(), "path", combinedPath)
		http.Error(
			w,
			fmt.Sprintf("Error getting path %s: %s", combinedPath, err.Error()),
			http.StatusBadRequest,
		)

		return
	}

	if !info.IsDir() {
		http.Error(w, "Expected directory not file: "+combinedPath, http.StatusBadRequest)

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

	name := filepath.Base(handler.Filename)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		http.Error(w, "Error uploading: invalid file name", http.StatusBadRequest)

		return
	}

	localFile, err := root.OpenFile(
		filepath.Join(relPath, name),
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		mountFileMode,
	)
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

	defer func() { _ = localFile.Close() }()

	_, _ = io.Copy(localFile, clientFile)
}

// CopyExperimentFileToMount copies a same-experiment file into a mounted VM directory.
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

	root, release, ok := openActiveMount(w, basePath)
	if !ok {
		return
	}

	defer release()

	var (
		query   = r.URL.Query()
		relDir  = mountRelPath(query.Get("path"))
		destDir = filepath.Join(basePath, relDir)
	)

	info, err := root.Stat(relDir)
	if err != nil {
		http.Error(w, "Error getting path "+destDir+": "+err.Error(), http.StatusBadRequest)

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

	destFile, err := root.OpenFile(
		filepath.Join(relDir, filepath.Base(source)),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		mountFileMode,
	)
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

// mountRelPath turns a client-supplied path inside a mount, such as "/" or
// "/etc/hosts", into a path relative to the mount root for use with [os.Root].
// [os.Root], not this function, is what keeps the result inside the mount: it
// refuses ".." elements and symlinks that resolve outside the root, which a
// guest file system is free to contain.
func mountRelPath(path string) string {
	rel := strings.TrimPrefix(filepath.Clean("/"+path), "/")
	if rel == "" {
		return "."
	}

	return rel
}

// mountFileMode is the mode of files uploaded or copied into a VM mount.
const mountFileMode = 0o600

// openActiveMount opens the active mount at basePath as an [os.Root] and holds
// the mount's read lock so it is not unmounted while in use. It answers the
// request itself on failure. The caller must call release when done.
func openActiveMount(w http.ResponseWriter, basePath string) (*os.Root, func(), bool) {
	activeMountsMu.RLock()

	mountInfo, exists := activeMounts[basePath]

	activeMountsMu.RUnlock()

	if !exists {
		http.Error(w, "No existing mount for "+basePath, http.StatusBadRequest)

		return nil, nil, false
	}

	mountInfo.lock.RLock()

	root, err := os.OpenRoot(basePath)
	if err != nil {
		mountInfo.lock.RUnlock()
		http.Error(w, "Error opening mount "+basePath+": "+err.Error(), http.StatusInternalServerError)

		return nil, nil, false
	}

	return root, func() {
		_ = root.Close()

		mountInfo.lock.RUnlock()
	}, true
}

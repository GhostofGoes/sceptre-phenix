package mm

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"phenix/util/common"
	"phenix/util/plog"
)

// filesDirectoryRetry is how often GetMMFullPath asks minimega for its files
// directory again while minimega has not reported one.
const filesDirectoryRetry = 10 * time.Second

// mmFiles caches the files directory minimega reports in `args`. It is read
// only once the command line has set the bases (see ResolveFilesDirectory): an
// earlier read would dial the default minimega base before --base-dir.minimega
// applies, and mmcli keeps that connection for every later command.
var mmFiles struct { //nolint:gochecknoglobals // process-lifetime cache
	mu      sync.Mutex
	dir     string    // minimega's answer, once it gave one
	enabled bool      // ResolveFilesDirectory ran, so GetMMFullPath may ask minimega
	asked   time.Time // when minimega was last asked
	warned  bool      // the fallback was logged since minimega last answered
}

// mmArgs reads minimega's `args`. It asks minimega itself, not DefaultMM:
// test doubles of DefaultMM know no files directory.
var mmArgs = Minimega{}.GetMMArgs //nolint:gochecknoglobals // overridden by tests

// ResolveFilesDirectory asks minimega for its files directory, which
// GetMMFullPath then uses. The command line calls it once it has set the
// bases. Until it runs, and while minimega has not answered, GetMMFullPath
// uses <phenix base>/images; after it ran, GetMMFullPath asks again every
// filesDirectoryRetry until minimega answers.
func ResolveFilesDirectory() {
	mmFiles.mu.Lock()
	mmFiles.enabled = true
	mmFiles.mu.Unlock()

	_ = GetMMFilesDirectory()
}

// GetMMFullPath returns the full path relative to the minimega files directory.
func GetMMFullPath(path string) string {
	// If there is no leading file separator, assume a relative
	// path to the minimega files directory
	if !strings.HasPrefix(path, "/") {
		return filepath.Join(filesDirectory(), path)
	} else {
		return path
	}
}

// filesDirectory returns the files directory minimega reported, or asks
// minimega again as ResolveFilesDirectory describes.
func filesDirectory() string {
	mmFiles.mu.Lock()

	dir := mmFiles.dir
	ask := dir == "" && mmFiles.enabled && time.Since(mmFiles.asked) >= filesDirectoryRetry

	mmFiles.mu.Unlock()

	switch {
	case dir != "":
		return dir
	case ask:
		return GetMMFilesDirectory()
	default:
		return defaultFilesDirectory()
	}
}

func defaultFilesDirectory() string {
	return common.PhenixBase + "/images"
}

// GetMMFilesDirectory asks minimega for its files directory, its -filepath,
// and remembers the answer for GetMMFullPath. While minimega does not answer
// it returns <phenix base>/images, logging that once.
func GetMMFilesDirectory() string {
	path, ok := "", false

	args, err := mmArgs()
	if err == nil {
		path, ok = args["filepath"]
	}

	mmFiles.mu.Lock()
	defer mmFiles.mu.Unlock()

	mmFiles.asked = time.Now()

	if ok && path != "" {
		mmFiles.dir, mmFiles.warned = path, false

		return path
	}

	fallback := defaultFilesDirectory()

	if !mmFiles.warned {
		mmFiles.warned = true

		plog.Warn(
			plog.TypeSystem,
			"could not get the files directory from minimega, using the default",
			"dir", fallback, "err", err,
		)
	}

	return fallback
}

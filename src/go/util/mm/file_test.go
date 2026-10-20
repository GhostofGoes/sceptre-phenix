package mm

import (
	"errors"
	"testing"
	"time"

	"phenix/util/common"
)

// TestGetMMFullPathReadsFilesDirectoryAfterTheBases is a regression test: the
// files directory was read from minimega's `args` at package init, before the
// command line set the bases, so a read that failed left /phenix/images for
// the life of the process even after minimega came up or with
// --base-dir.phenix set, and one that succeeded asked the default minimega
// base before --base-dir.minimega applied.
func TestGetMMFullPathReadsFilesDirectoryAfterTheBases(t *testing.T) {
	originalArgs, originalBase := mmArgs, common.PhenixBase

	t.Cleanup(func() {
		mmArgs, common.PhenixBase = originalArgs, originalBase //nolint:reassign // restore test fixture

		mmFiles.mu.Lock()
		mmFiles.dir, mmFiles.enabled, mmFiles.asked, mmFiles.warned = "", false, time.Time{}, false
		mmFiles.mu.Unlock()
	})

	var (
		calls  int
		answer map[string]string
	)

	mmArgs = func() (map[string]string, error) {
		calls++

		if answer == nil {
			return nil, errors.New("unable to dial")
		}

		return answer, nil
	}

	common.PhenixBase = "/custom/phenix" //nolint:reassign // test fixture

	// Before the command line set the bases, minimega is not asked.
	if got := GetMMFullPath("x.qc2"); got != "/custom/phenix/images/x.qc2" || calls != 0 {
		t.Errorf("before ResolveFilesDirectory: GetMMFullPath = %q after %d args calls, want the fallback and none", got, calls)
	}

	ResolveFilesDirectory() // minimega is down

	if got := GetMMFullPath("x.qc2"); got != "/custom/phenix/images/x.qc2" || calls != 1 {
		t.Errorf("minimega down: GetMMFullPath = %q after %d args calls, want the fallback and 1", got, calls)
	}

	// minimega comes up; it is asked again once the retry interval passed.
	answer = map[string]string{"filepath": "/srv/mm/files"}

	mmFiles.mu.Lock()
	mmFiles.asked = time.Time{}
	mmFiles.mu.Unlock()

	for range 2 {
		if got := GetMMFullPath("x.qc2"); got != "/srv/mm/files/x.qc2" {
			t.Errorf("minimega up: GetMMFullPath = %q, want /srv/mm/files/x.qc2", got)
		}
	}

	if calls != 2 {
		t.Errorf("minimega was asked %d times, want 2: its answer is kept", calls)
	}

	if got := GetMMFullPath("/abs/y.qc2"); got != "/abs/y.qc2" {
		t.Errorf("GetMMFullPath of an absolute path = %q", got)
	}
}

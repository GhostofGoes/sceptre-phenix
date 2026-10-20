package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"

	"phenix/store"
	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

// TestSnapshotVMFailureLeavesNoGoroutine checks that a failed snapshot leaves no
// goroutine behind: SnapshotVM must end everything it started.
func TestSnapshotVMFailureLeavesNoGoroutine(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	m := store.NewMockStore(ctrl)
	m.EXPECT().Get(gomock.Any()).Return(errors.New("store unavailable")).AnyTimes()

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double
	store.DefaultStore = m                              //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"vms/snapshots"},
					ResourceNames: []string{"*/*"},
					Verbs:         []string{"create"},
				},
			},
		},
	}

	snapshot := func() {
		t.Helper()

		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/experiments/test-experiment/vms/test-vm/snapshots",
			strings.NewReader(`{"filename":"s1"}`),
		)
		req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "test-vm"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		SnapshotVM(rec, req)

		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "store unavailable") {
			t.Fatalf("SnapshotVM() = %d %q, want 500 with the store error", rec.Code, rec.Body.String())
		}
	}

	// The first call may start goroutines that live on, such as for logging.
	snapshot()

	before := runtime.NumGoroutine()

	const calls = 5

	for range calls {
		snapshot()
	}

	after := runtime.NumGoroutine()

	for deadline := time.Now().Add(time.Second); after > before && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)

		after = runtime.NumGoroutine()
	}

	if after > before {
		t.Errorf("%d failed snapshots left %d goroutines behind", calls, after-before)
	}
}

// TestVMFileNameHandlersRefuseUnsafeNames checks that the routes that name a
// file after the request answer 400 for a name minimega or QEMU would run or
// split, before they look anything up: such names reached a root `shell rm -f`
// and QEMU's `exec:` migration URI.
func TestVMFileNameHandlersRefuseUnsafeNames(t *testing.T) {
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	// Any lookup fails the test: the name must be refused first.
	m := store.NewMockStore(ctrl)

	original := store.DefaultStore
	t.Cleanup(func() { store.DefaultStore = original }) //nolint:reassign // restore test double
	store.DefaultStore = m                              //nolint:reassign // install test double

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{
				{
					Resources:     []string{"vms/snapshots", "vms/memorySnapshot", "vms/captures"},
					ResourceNames: []string{"*/*"},
					Verbs:         []string{"create"},
				},
			},
		},
	}

	for _, tc := range []struct {
		route   string
		body    string
		handler http.HandlerFunc
	}{
		{"snapshots", `{"filename":"x -rf /var/lib/phenix y"}`, SnapshotVM},
		{"snapshots", `{"filename":"a;reboot;true${IFS}"}`, SnapshotVM},
		{"memorySnapshot", `{"filename":"/root/.ssh/authorized_keys"}`, CreateVMMemorySnapshot},
		{"captures", `{"interface":0,"filename":"a\\\"b.pcap"}`, StartVMCapture},
	} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/experiments/test-experiment/vms/test-vm/"+tc.route,
			strings.NewReader(tc.body),
		)
		req = mux.SetURLVars(req, map[string]string{"exp": "test-experiment", "name": "test-vm"})
		req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

		rec := httptest.NewRecorder()
		tc.handler(rec, req)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid file name") {
			t.Errorf("POST %s %s = %d %q, want 400 naming the invalid file name", tc.route, tc.body, rec.Code, rec.Body.String())
		}
	}
}

// TestCommitAndMemorySnapshotLeaveNoGoroutine is a regression test: CommitVM
// started a progress goroutine ranging over a channel nothing closed, and
// CreateVMMemorySnapshot one that waited for a "failed" or "completed" that
// MemorySnapshot does not send when it refuses before its poll, so every such
// request left a goroutine behind for the life of phenix ui.
func TestCommitAndMemorySnapshotLeaveNoGoroutine(t *testing.T) {
	useVMTestStore(t, "") // stopped, so both refuse at once

	role := rbac.Role{
		Spec: &v1.RoleSpec{
			Policies: []*v1.PolicySpec{{
				Resources:     []string{"vms/commit", "vms/memorySnapshot"},
				ResourceNames: []string{"*/*"},
				Verbs:         []string{"create"},
			}},
		},
	}

	for _, h := range []struct {
		path, body string
		handler    http.HandlerFunc
		frame      string
	}{
		{"/commit", `{"filename":"new.qc2"}`, CommitVM, "web.CommitVM"},
		{"/memorySnapshot", `{"filename":"mem.dump"}`, CreateVMMemorySnapshot, "web.CreateVMMemorySnapshot"},
	} {
		for range 3 {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/experiments/exp1/vms/vm1"+h.path, strings.NewReader(h.body))
			req = mux.SetURLVars(req, map[string]string{"exp": "exp1", "name": "vm1"})
			req = req.WithContext(context.WithValue(req.Context(), middleware.ContextKeyRole, role))

			rec := httptest.NewRecorder()
			h.handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST %s = %d %q, want 400 for a stopped experiment", h.path, rec.Code, rec.Body.String())
			}
		}

		stacks := make([]byte, 1<<20)
		stacks = stacks[:runtime.Stack(stacks, true)]

		if strings.Contains(string(stacks), h.frame+".func") {
			t.Errorf("POST %s left a goroutine of %s behind:\n%s", h.path, h.frame, stacks)
		}
	}
}

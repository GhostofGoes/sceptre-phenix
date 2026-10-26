package scorch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"

	v1 "phenix/types/version/v1"
	"phenix/web/middleware"
	"phenix/web/rbac"
)

const testExp = "exp"

func scorchTestRole(policies ...*v1.PolicySpec) rbac.Role {
	return rbac.Role{Spec: &v1.RoleSpec{Policies: policies}}
}

func viewerPolicy() *v1.PolicySpec {
	return &v1.PolicySpec{Resources: []string{"experiments"}, ResourceNames: []string{testExp}, Verbs: []string{"get"}}
}

func triggerPolicy() *v1.PolicySpec {
	return &v1.PolicySpec{Resources: []string{"experiments/trigger"}, ResourceNames: []string{testExp}, Verbs: []string{"create"}}
}

// otherExpRole may do anything, but only to a different experiment.
func otherExpRole() rbac.Role {
	return scorchTestRole(&v1.PolicySpec{
		Resources:     []string{"experiments", "experiments/*"},
		ResourceNames: []string{"other"},
		Verbs:         []string{"*"},
	})
}

func scorchRequest(method string, vars map[string]string, role rbac.Role) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	req = mux.SetURLVars(req, vars)

	ctx := context.WithValue(req.Context(), middleware.ContextKeyRole, role)
	ctx = context.WithValue(ctx, middleware.ContextKeyUser, "test-user")

	return req.WithContext(ctx)
}

// installTestTerminal registers a break terminal for the test experiment
// without spawning a process, and removes it when the test ends.
func installTestTerminal(t *testing.T) WebTerm {
	t.Helper()

	term := newWebTerm(testExp, 0, 0, "break", "shell")
	term.Pid = 999999

	webTermMu.Lock()
	webTermsPid[term.Pid] = term
	webTermsExp[term.key] = term
	webTermMu.Unlock()

	t.Cleanup(func() {
		webTermMu.Lock()
		delete(webTermsPid, term.Pid)
		delete(webTermsExp, term.key)
		webTermMu.Unlock()

		mu.Lock()
		delete(rwTerm, term.Pid)
		mu.Unlock()
	})

	return term
}

func connectVars() map[string]string {
	return map[string]string{"name": testExp, "run": "0", "loop": "0", "stage": "break", "cmp": "shell"}
}

func TestScorchEndpointsRejectUsersWithoutExperimentAccess(t *testing.T) {
	installTestTerminal(t)

	tests := map[string]func(http.ResponseWriter, *http.Request){
		"GetTerminals":          GetTerminals,
		"ConnectTerminal":       ConnectTerminal,
		"StreamTerminal":        StreamTerminal,
		"ExitTerminal":          ExitTerminal,
		"StreamComponentOutput": StreamComponentOutput,
	}

	vars := connectVars()
	vars["pid"] = "999999"
	vars["id"] = "anything"

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler(rec, scorchRequest(http.MethodGet, vars, otherExpRole()))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("GetComponentOutput", func(t *testing.T) {
		rec := httptest.NewRecorder()

		if err := GetComponentOutput(rec, scorchRequest(http.MethodGet, vars, otherExpRole())); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})
}

func TestGetTerminalsChecksEachListedExperiment(t *testing.T) {
	installTestTerminal(t)

	// A wildcard name matches the comma-joined list as a whole, so each
	// experiment in the list must be authorized on its own.
	role := scorchTestRole(&v1.PolicySpec{
		Resources:     []string{"experiments"},
		ResourceNames: []string{"ex*"},
		Verbs:         []string{"get"},
	})

	rec := httptest.NewRecorder()
	GetTerminals(rec, scorchRequest(http.MethodGet, map[string]string{"name": testExp + ",secret"}, role))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	GetTerminals(rec, scorchRequest(http.MethodGet, map[string]string{"name": testExp}, role))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConnectTerminalGivesViewersReadOnlyAccess(t *testing.T) {
	term := installTestTerminal(t)

	rec := httptest.NewRecorder()
	ConnectTerminal(rec, scorchRequest(http.MethodGet, connectVars(), scorchTestRole(viewerPolicy())))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	_, owned := rwTerm[term.Pid]
	mu.Unlock()

	if owned {
		t.Fatal("viewer was granted read/write ownership of the break terminal")
	}

	// A user who may trigger SCORCH still gets read/write access.
	rec = httptest.NewRecorder()
	ConnectTerminal(rec, scorchRequest(http.MethodGet, connectVars(), scorchTestRole(viewerPolicy(), triggerPolicy())))

	mu.Lock()
	_, owned = rwTerm[term.Pid]
	mu.Unlock()

	if !owned {
		t.Fatal("user with trigger permission was not granted read/write access")
	}
}

func TestExitTerminalRequiresTriggerPermission(t *testing.T) {
	term := installTestTerminal(t)

	mu.Lock()
	rwTerm[term.Pid] = "owner-id"
	mu.Unlock()

	vars := map[string]string{"name": testExp, "pid": "999999", "id": "owner-id"}

	rec := httptest.NewRecorder()
	ExitTerminal(rec, scorchRequest(http.MethodPost, vars, scorchTestRole(viewerPolicy())))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadOnlyClientTimeoutKeepsWriterOwnership(t *testing.T) {
	term := installTestTerminal(t)

	mu.Lock()
	rwTerm[term.Pid] = "writer-id"
	mu.Unlock()

	// A read-only client that never opens its websocket times out.
	if _, err := initTerminal(testExp, 0, 0, "break", "shell", false); err != nil {
		t.Fatal(err)
	}

	time.Sleep(TerminalInitTimeout + 500*time.Millisecond)

	mu.Lock()
	owner := rwTerm[term.Pid]
	mu.Unlock()

	if owner != "writer-id" {
		t.Fatalf("writer ownership was dropped by a read-only client's timeout, owner now %q", owner)
	}
}

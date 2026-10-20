package forward

import (
	"testing"

	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
)

// caseFoldMM lists VM1 on h2 before vm1 on h1 for vm1, as minimega's
// case-folding name filter does, and creates every tunnel asked for.
type caseFoldMM struct {
	mm.MM
}

func (caseFoldMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{{Name: "VM1", Host: "h2"}, {Name: "vm1", Host: "h1"}}
}

func (caseFoldMM) CreateTunnel(...mm.Option) error {
	return nil
}

// TestCreatePortForwardUsesTheHostOfTheExactVM is a regression test: a port
// forward took the host of the first VM minimega's case-folding name filter
// listed, so with VMs VM1 and vm1 a forward to vm1 connected to VM1's host.
func TestCreatePortForwardUsesTheHostOfTheExactVM(t *testing.T) {
	installExperimentStore(t, map[string]string{"exp": "2026-10-20T00:00:00Z"}, nil)

	originalMM, originalForwards := mm.DefaultMM, forwards

	t.Cleanup(func() {
		mm.DefaultMM = originalMM //nolint:reassign // restore test double

		forwardsMu.Lock()
		forwards = originalForwards
		forwardsMu.Unlock()
	})

	mm.DefaultMM = caseFoldMM{} //nolint:reassign // install test double

	forwardsMu.Lock()
	forwards = make(map[string]ft.Listener)
	forwardsMu.Unlock()

	if err := createPortForward("exp", "vm1", "8080", "10.0.0.1", "22", "user"); err != nil {
		t.Fatalf("createPortForward() = %v", err)
	}

	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	key := ft.Listener{Exp: "exp", VM: "vm1", DstHost: "10.0.0.1", DstPort: 22, Owner: "user"}.ToKey()

	if got := forwards[key].ClusterHost; got != "h1" {
		t.Errorf("the forward to vm1 goes through host %q, want h1", got)
	}
}

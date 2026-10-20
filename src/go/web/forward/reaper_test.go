package forward

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hashicorp/go-multierror"

	"phenix/util/mm"
	"phenix/util/mm/mmcli"
	ft "phenix/web/forward/forwardtypes"
)

// reaperTestMM is a test double for mm.MM that answers tunnel listings.
// Embedding the (nil) mm.MM interface means any other method call panics.
type reaperTestMM struct {
	mm.MM

	rows []map[string]string
	err  error
}

func (m *reaperTestMM) GetTunnels(...mm.Option) ([]map[string]string, error) {
	return m.rows, m.err
}

// minimegaErrs builds the error mm.GetTunnels returns for the given minimega
// error responses.
func minimegaErrs(msgs ...string) error {
	var errs error

	for _, msg := range msgs {
		errs = multierror.Append(errs, errors.New(msg))
	}

	return fmt.Errorf("listing tunnels of c2b in namespace exp: %w", errs)
}

func TestReaperKeepsForwardOnListingError(t *testing.T) {
	row := []map[string]string{{"vm": "c2b", "id": "0", "dst": "127.0.0.1", "dst port": "8080"}}

	for _, tc := range []struct {
		name string
		rows []map[string]string
		err  error
		keep bool
	}{
		{"tunnel listed", row, nil, true},
		{"tunnel closed", nil, nil, false},
		{"lost connection", nil, fmt.Errorf("listing tunnels: %w", mmcli.ErrNoResponse), true},
		{"timeout", nil, minimegaErrs(mmcli.ErrTimeout.Error()), true},
		{"vm not found", nil, minimegaErrs("vm not found: c2b"), false},
		{"client expired", nil, minimegaErrs("no such client: 1234"), false},
		{"tunnel not initialized", nil, minimegaErrs("tunnel has not been initialized for 1234"), false},
		{"other host failed", row, minimegaErrs("vm not found: c2b"), true},
		{"one host unreachable", nil, minimegaErrs("vm not found: c2b", "unable to dial: refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The reaper asks about the tunnels of running experiments only.
			installExperimentStore(t, map[string]string{"exp": "2024-01-01T00:00:00Z"}, nil)

			original := mm.DefaultMM
			t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

			mm.DefaultMM = &reaperTestMM{rows: tc.rows, err: tc.err} //nolint:reassign // install test double

			l := ft.Listener{
				Exp: "exp", VM: "c2b", SrcPort: 8080, DstHost: "127.0.0.1", DstPort: 8080, Owner: "bot",
			}

			forwardsMu.Lock()
			defer forwardsMu.Unlock()

			forwards[l.ToKey()] = l

			reapForwards()

			if _, ok := forwards[l.ToKey()]; ok != tc.keep {
				t.Errorf("forward kept = %t, want %t", ok, tc.keep)
			}

			delete(forwards, l.ToKey())
		})
	}
}

func TestReaperKeepsVNCForward(t *testing.T) {
	installExperimentStore(t, map[string]string{"exp": "2024-01-01T00:00:00Z"}, nil)

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	// A VNC forward has no minimega tunnel; listing tunnels would panic.
	mm.DefaultMM = nil //nolint:reassign // install test double

	l := ft.Listener{
		Exp: "exp", VM: "c2b", SrcPort: 5901, DstHost: "127.0.0.1", DstPort: 5900, QEMU: true,
	}

	forwardsMu.Lock()
	defer forwardsMu.Unlock()

	forwards[l.ToKey()] = l

	reapForwards()

	if _, ok := forwards[l.ToKey()]; !ok {
		t.Error("reaper dropped a VNC forward")
	}

	delete(forwards, l.ToKey())
}

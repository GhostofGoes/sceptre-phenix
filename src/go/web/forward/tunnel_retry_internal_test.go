package forward

import (
	"errors"
	"fmt"
	"testing"

	"phenix/util/mm"
)

// tunnelRetryMM lists vm1 and answers every tunnel request with err.
type tunnelRetryMM struct {
	mm.MM

	err   error
	calls *int
}

func (tunnelRetryMM) GetVMInfo(...mm.Option) mm.VMs {
	return mm.VMs{{Name: "vm1", Host: "head"}}
}

func (m tunnelRetryMM) CreateTunnel(...mm.Option) error {
	*m.calls++

	return m.err
}

// TestCreatePortForwardBoundsPortRetries is a regression test: a forward
// retried another local port for as long as the tunnel error held `bind:
// address already in use`, with the lock of every forward request held, and a
// host such as `a bind: address already in use` put that text into every
// error, so one request kept phenix ui busy until it restarted.
func TestCreatePortForwardBoundsPortRetries(t *testing.T) {
	installExperimentStore(t, map[string]string{"exp": "2026-10-18T00:00:00Z"}, nil)

	original := mm.DefaultMM

	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	const inUse = "listen tcp :50001: bind: address already in use"

	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"port always taken", errors.New(inUse), maxTunnelAttempts},
		{"refused host", fmt.Errorf("%s: %w", inUse, mm.ErrUnsafeArgument), 1},
		{"other error", errors.New("no such VM"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int

			mm.DefaultMM = tunnelRetryMM{err: tc.err, calls: &calls} //nolint:reassign // install test double

			if err := createPortForward("exp", "vm1", "8080", "10.0.0.1", "22", "user"); !errors.Is(err, tc.err) {
				t.Errorf("createPortForward() = %v, want %v", err, tc.err)
			}

			if calls != tc.calls {
				t.Errorf("createPortForward() tried %d tunnels, want %d", calls, tc.calls)
			}
		})
	}
}

package forward

import (
	"testing"

	"phenix/util/mm"
	ft "phenix/web/forward/forwardtypes"
)

// tunnelsMM is a test double for mm.MM that only answers GetTunnels, with the
// same tunnels for every VM. Any other method call panics on the embedded nil
// interface.
type tunnelsMM struct {
	mm.MM

	tunnels []map[string]string
	calls   int
}

func (m *tunnelsMM) GetTunnels(...mm.Option) []map[string]string {
	m.calls++

	return m.tunnels
}

func TestForwardExistsMatchesDestination(t *testing.T) {
	t.Parallel()

	tunnels := []map[string]string{
		{"vm": "a", "id": "1", "src port": "50001", "dst": "127.0.0.1", "dst port": "22"},
		{"vm": "a", "id": "2", "src port": "50002", "dst": "Router.Local", "dst port": "80"},
	}

	for name, tc := range map[string]struct {
		listener ft.Listener
		want     bool
	}{
		"exact":             {ft.Listener{DstHost: "127.0.0.1", DstPort: 22}, true},
		"host folds case":   {ft.Listener{DstHost: "router.local", DstPort: 80}, true},
		"wrong port":        {ft.Listener{DstHost: "127.0.0.1", DstPort: 80}, false},
		"wrong host":        {ft.Listener{DstHost: "10.0.0.1", DstPort: 22}, false},
		"port unset":        {ft.Listener{DstHost: "127.0.0.1"}, true},
		"qemu needs none":   {ft.Listener{QEMU: true}, true},
		"no tunnels listed": {ft.Listener{DstHost: "127.0.0.1", DstPort: 22, VM: "none"}, true},
	} {
		rows := tunnels
		if tc.listener.VM == "none" {
			rows, tc.want = nil, false
		}

		if got := forwardExists(tc.listener, rows); got != tc.want {
			t.Errorf("%s: forwardExists = %v, want %v", name, got, tc.want)
		}
	}
}

func TestReapForwardsListsTunnelsOncePerVM(t *testing.T) { //nolint:paralleltest // replaces package state
	fake := &tunnelsMM{tunnels: []map[string]string{
		{"dst": "127.0.0.1", "dst port": "22"},
		{"dst": "127.0.0.1", "dst port": "80"},
	}}

	original := mm.DefaultMM
	mm.DefaultMM = fake //nolint:reassign // install test double

	forwardsMu.Lock()
	saved := forwards
	forwards = make(map[string]ft.Listener)

	for _, l := range []ft.Listener{
		{Exp: "exp", VM: "a", DstHost: "127.0.0.1", DstPort: 22, Owner: "u"},
		{Exp: "exp", VM: "a", DstHost: "127.0.0.1", DstPort: 80, Owner: "u"},
		{Exp: "exp", VM: "b", DstHost: "127.0.0.1", DstPort: 22, Owner: "u"},
		{Exp: "exp", VM: "c", DstPort: 5900, Owner: "u", QEMU: true},
	} {
		forwards[l.ToKey()] = l
	}

	reapForwards()

	remaining := len(forwards)
	forwards = saved
	forwardsMu.Unlock()

	mm.DefaultMM = original //nolint:reassign // restore test double

	if remaining != 4 {
		t.Fatalf("%d forwards remain, want all 4", remaining)
	}

	if fake.calls != 2 {
		t.Fatalf("listed tunnels %d times, want once per VM (2)", fake.calls)
	}
}

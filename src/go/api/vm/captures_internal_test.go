package vm

import (
	"reflect"
	"testing"

	"phenix/util/mm"
)

// experimentCapturesMM is a test double for mm.MM that only answers
// GetExperimentCaptures. Any other method call panics on the embedded nil
// interface.
type experimentCapturesMM struct {
	mm.MM

	captures []mm.Capture
	calls    int
}

func (m *experimentCapturesMM) GetExperimentCaptures(...mm.Option) []mm.Capture {
	m.calls++

	return m.captures
}

func TestCapturesForVMsListsCapturesOnce(t *testing.T) { //nolint:paralleltest // replaces mm.DefaultMM
	fake := &experimentCapturesMM{captures: []mm.Capture{
		{VM: "a", Interface: 0, Filepath: "a0"},
		{VM: "b", Interface: 0, Filepath: "b0"},
		{VM: "a", Interface: 1, Filepath: "a1"},
		{VM: "c", Interface: 0, Filepath: "c0"},
	}}

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = fake //nolint:reassign // install test double

	// A VM matched on two interfaces is named twice, and its captures were
	// listed twice before; keep that.
	got := capturesForVMs("exp", []string{"b", "a", "b", "missing"})
	want := []mm.Capture{
		{VM: "b", Interface: 0, Filepath: "b0"},
		{VM: "a", Interface: 0, Filepath: "a0"},
		{VM: "a", Interface: 1, Filepath: "a1"},
		{VM: "b", Interface: 0, Filepath: "b0"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capturesForVMs = %v, want %v", got, want)
	}

	if fake.calls != 1 {
		t.Fatalf("listed captures %d times, want 1", fake.calls)
	}

	if got := capturesForVMs("exp", nil); got != nil || fake.calls != 1 {
		t.Fatalf("no VMs: got %v after %d listings, want nil without listing", got, fake.calls)
	}
}

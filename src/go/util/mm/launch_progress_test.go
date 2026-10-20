package mm

import (
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/activeshadow/libminimega/minicli"
)

// progressMinimega is a fake minimega listing the namespaces in listed, whose
// namespace "q" queues three VMs.
type progressMinimega struct {
	fakeMinimega

	listedMu sync.Mutex
	listed   []string
}

func newProgressMinimega(listed ...string) *progressMinimega {
	f := &progressMinimega{listed: listed}
	f.answer = f.answerProgress

	return f
}

func (f *progressMinimega) list(ns string) {
	f.listedMu.Lock()
	defer f.listedMu.Unlock()

	f.listed = append(f.listed, ns)
}

func (f *progressMinimega) answerProgress(cmd string) minicli.Responses {
	f.listedMu.Lock()
	defer f.listedMu.Unlock()

	switch {
	case cmd == "namespace":
		rows := make([][]string, 0, len(f.listed))
		for _, ns := range f.listed {
			rows = append(rows, []string{ns, "", "false"})
		}

		return minicli.Responses{{Host: "head", Header: []string{"namespace", "vlans", "active"}, Tabular: rows}}
	case strings.HasSuffix(cmd, "ns queue"):
		return minicli.Responses{{Host: "head", Response: "VMs: 3\nNames: q[1-3]\n"}}
	default:
		return minicli.Responses{{Host: "head", Error: "unexpected command"}}
	}
}

// TestGetLaunchProgressLeavesAbsentNamespacesAlone is a regression test:
// phenix ui polls the launch progress from the moment a REST start begins, and
// the poll ran `ns queue` in the experiment's namespace, which minimega does not
// list before the start reads its script, or after a failed start cleared it,
// and so created it. A start that failed before minimega launched anything,
// such as in a pre-start app, then left the namespace behind.
func TestGetLaunchProgressLeavesAbsentNamespacesAlone(t *testing.T) {
	fake := newProgressMinimega("minimega")
	fake.serve(t)

	p, err := Minimega{}.GetLaunchProgress("q", 3)
	if err != nil || p != 0 {
		t.Errorf("GetLaunchProgress of an absent namespace = %v (%v), want 0", p, err)
	}

	if sent := fake.sent(); slices.ContainsFunc(sent, func(c string) bool { return strings.HasPrefix(c, "namespace ") }) {
		t.Errorf("GetLaunchProgress entered the absent namespace: %q", sent)
	}

	fake.list("q")

	p, err = Minimega{}.GetLaunchProgress("q", 3)
	if err != nil || math.Abs(p-1) > 1e-9 {
		t.Errorf("GetLaunchProgress with 3 of 3 VMs queued = %v (%v), want 1", p, err)
	}
}

// TestGetLaunchProgressHoldsOffClearNamespace is a regression test: a failed
// REST start cleared the namespace between the progress poll's check that
// minimega lists it and the poll's `ns queue` in it, which created the
// namespace again and left it behind.
func TestGetLaunchProgressHoldsOffClearNamespace(t *testing.T) {
	var (
		fake    = newProgressMinimega("minimega", "q")
		listing = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)

	answer := fake.answer
	fake.answer = func(cmd string) minicli.Responses {
		if cmd == "namespace" {
			once.Do(func() {
				close(listing)
				<-release
			})
		}

		if strings.HasPrefix(cmd, "clear namespace") {
			return minicli.Responses{{Host: "head"}}
		}

		return answer(cmd)
	}

	fake.serve(t)

	polled := make(chan error, 1)

	go func() {
		_, err := Minimega{}.GetLaunchProgress("q", 3)
		polled <- err
	}()

	<-listing

	cleared := make(chan error, 1)

	go func() { cleared <- Minimega{}.ClearNamespace("q") }()

	// Give the clear time to queue up behind the poll's listing.
	time.Sleep(100 * time.Millisecond)
	close(release)

	if err := <-polled; err != nil {
		t.Fatalf("GetLaunchProgress: %v", err)
	}

	if err := <-cleared; err != nil {
		t.Fatalf("ClearNamespace: %v", err)
	}

	want := []string{"namespace", `namespace "q" ns queue`, "clear namespace q"}
	if got := fake.sent(); !slices.Equal(got, want) {
		t.Errorf("minimega got %q, want %q: the clear must not run inside the poll", got, want)
	}
}

package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"phenix/types"
	"phenix/util/mm"
	"phenix/util/plog"

	v1 "phenix/types/version/v1"
)

const autotunnelTestStart = "2026-10-19T00:00:00Z"

// autotunnelTestMM fails the test on any minimega call: phenix ui, not the
// startup app, creates autotunnels.
type autotunnelTestMM struct {
	mm.MM

	t *testing.T
}

func (m autotunnelTestMM) IsC2ClientActive(...mm.C2Option) error {
	m.t.Error("the startup app asked minimega about a VM's C2 client")

	return nil
}

// syncBuffer is a [bytes.Buffer] the log handler may write to while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func captureAutotunnelLogs(t *testing.T) *syncBuffer {
	t.Helper()

	logs := new(syncBuffer)

	plog.AddHandler(t.Name(), slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { plog.RemoveHandler(t.Name()) })

	return logs
}

func installAutotunnelTestMM(t *testing.T) {
	t.Helper()

	original := mm.DefaultMM
	t.Cleanup(func() { mm.DefaultMM = original }) //nolint:reassign // restore test double

	mm.DefaultMM = autotunnelTestMM{t: t} //nolint:reassign // install test double
}

// newAutotunnelExperiment returns a started experiment whose VM web carries
// the autotunnel annotation value.
func newAutotunnelExperiment(t *testing.T, annotation any) *types.Experiment {
	t.Helper()

	node := newStartupTestNode(t, "web", "linux", true, 1)
	node.AnnotationsF = map[string]any{AutotunnelAnnotation: annotation}

	exp, _ := newStartupTestExperiment(t, node, nil)
	exp.Status.SetStartTime(autotunnelTestStart)

	return exp
}

func TestAutotunnels(t *testing.T) {
	tunnel := func(sport, dhost, dport string) Autotunnel {
		return Autotunnel{Experiment: "exp1", VM: "web", Sport: sport, Dhost: dhost, Dport: dport}
	}

	for _, tc := range []struct {
		name       string
		annotation any
		want       []Autotunnel
		errs       []string
	}{
		{
			name:       "every form",
			annotation: []any{"8080", "8081:9090", "8082:10.0.0.5:80", "5901:VNC"},
			want: []Autotunnel{
				tunnel("8080", "127.0.0.1", "8080"),
				tunnel("8081", "127.0.0.1", "9090"),
				tunnel("8082", "10.0.0.5", "80"),
				tunnel("5901", "127.0.0.1", "VNC"),
			},
		},
		{
			name:       "invalid entries are skipped",
			annotation: []string{"a:b:c:d", "http", "8080:http", "0", "8080::80", "9000"},
			want:       []Autotunnel{tunnel("9000", "127.0.0.1", "9000")},
			errs: []string{
				`entry "a:b:c:d": want sport`,
				`entry "http": invalid source port`,
				`entry "8080:http": invalid destination port`,
				`entry "0": invalid source port`,
				`entry "8080::80": empty destination host`,
			},
		},
		{
			name:       "a repeated destination is skipped",
			annotation: []string{"8080", "9080:8080", "9081:127.0.0.1:8080"},
			want:       []Autotunnel{tunnel("8080", "127.0.0.1", "8080")},
			errs: []string{
				`entry "9080:8080": forwards to the destination of entry "8080"`,
				`entry "9081:127.0.0.1:8080": forwards to the destination of entry "8080"`,
			},
		},
		{
			name:       "not a list",
			annotation: map[string]any{"port": 8080},
			errs:       []string{"vm web: parsing phenix/startup-autotunnel annotation"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := Autotunnels(newAutotunnelExperiment(t, tc.annotation))

			if len(got) != len(tc.want) {
				t.Fatalf("Autotunnels() = %+v, want %+v", got, tc.want)
			}

			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Autotunnels()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}

			if len(errs) != len(tc.errs) {
				t.Fatalf("Autotunnels() errors = %v, want %d containing %q", errs, len(tc.errs), tc.errs)
			}

			for i, err := range errs {
				if !strings.Contains(err.Error(), tc.errs[i]) {
					t.Errorf("Autotunnels() error %d = %v, want it to contain %q", i, err, tc.errs[i])
				}
			}
		})
	}
}

func TestAutotunnelsSkipExternalNodes(t *testing.T) {
	exp := newAutotunnelExperiment(t, []string{"8080"})

	node, _ := exp.Spec.Topology().Nodes()[0].(*v1.Node)
	node.ExternalF = boolPtr(true)

	if got, errs := Autotunnels(exp); len(got) != 0 || len(errs) != 0 {
		t.Errorf("Autotunnels() of an external node = %+v, %v; want none", got, errs)
	}
}

func TestAutotunnelDstPort(t *testing.T) {
	for dport, want := range map[string]int{"8080": 8080, "VNC": 5900, "vnc": 5900} {
		if got := (Autotunnel{Dport: dport}).DstPort(); got != want {
			t.Errorf("DstPort() of %q = %d, want %d", dport, got, want)
		}
	}
}

// TestStartupPostStartLogsAutotunnels covers what the startup app does for
// autotunnels, whichever process starts the experiment: it logs each entry
// phenix ui will create, and each it ignores, and asks minimega nothing.
func TestStartupPostStartLogsAutotunnels(t *testing.T) {
	installAutotunnelTestMM(t)

	logs := captureAutotunnelLogs(t)

	if err := (Startup{}).PostStart(context.Background(), newAutotunnelExperiment(t, []string{"8080", "http"})); err != nil {
		t.Fatalf("running startup PostStart: %v", err)
	}

	for _, want := range []string{
		"startup autotunnel requested: phenix ui creates it once the VM's C2 client registers",
		"exp=exp1 vm=web sport=8080 dhost=127.0.0.1 dport=8080",
		"invalid phenix/startup-autotunnel annotation",
		`entry \"http\": invalid source port`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}

	if err := (Startup{}).Cleanup(context.Background(), newAutotunnelExperiment(t, []string{"8080"})); err != nil {
		t.Errorf("running startup Cleanup: %v", err)
	}
}

// TestStartupAutotunnelSkipsDryRuns is a regression test: the startup app
// asked minimega about a dry run's VM, which made minimega create the
// experiment's namespace. A dry run gets no autotunnels.
func TestStartupAutotunnelSkipsDryRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dryRun bool
		start  string
	}{
		{name: "dry-run option", dryRun: true, start: autotunnelTestStart},
		{name: "dry-run start time", start: autotunnelTestStart + "-DRYRUN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installAutotunnelTestMM(t)

			logs := captureAutotunnelLogs(t)

			exp := newAutotunnelExperiment(t, []string{"8080"})
			exp.Status.SetStartTime(tc.start)

			app := new(Startup)
			if err := app.Init(DryRun(tc.dryRun)); err != nil {
				t.Fatalf("initializing startup app: %v", err)
			}

			if err := app.PostStart(context.Background(), exp); err != nil {
				t.Fatalf("running startup PostStart: %v", err)
			}

			if strings.Contains(logs.String(), "startup autotunnel") {
				t.Errorf("logged an autotunnel for a dry run:\n%s", logs.String())
			}
		})
	}
}

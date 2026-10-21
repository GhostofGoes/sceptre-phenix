package mm

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/activeshadow/libminimega/miniclient"
	"github.com/hashicorp/go-multierror"
)

// TestStartEachContinuesAfterAFailure is a regression test: with an external
// or delayed node phenix starts VMs one `vm start <name>` at a time, and
// LaunchVMs returned at the first VM minimega failed to start, so the VMs
// after it were launched but never started.
func TestStartEachContinuesAfterAFailure(t *testing.T) {
	var started []string

	err := startEach([]string{"f1", "f2", "f3"}, func(name string) error {
		started = append(started, name)

		if name == "f1" {
			return errors.New("unable to connect to qmp socket")
		}

		return nil
	})

	if want := []string{"f1", "f2", "f3"}; !reflect.DeepEqual(started, want) {
		t.Errorf("startEach started %v, want %v", started, want)
	}

	var merr *multierror.Error
	if !errors.As(err, &merr) || len(merr.Errors) != 1 {
		t.Fatalf("startEach returned %v, want one error", err)
	}

	if got, want := merr.Errors[0].Error(), "starting VM f1: unable to connect to qmp socket"; got != want {
		t.Errorf("startEach error = %q, want %q", got, want)
	}
}

func TestStartEachWithoutFailures(t *testing.T) {
	if err := startEach([]string{"f1", "f2"}, func(string) error { return nil }); err != nil {
		t.Errorf("startEach returned %v, want nil", err)
	}
}

// hostResponses returns a response channel holding one `vm flush` answer per
// host, as minimega sends for a command it broadcasts to a namespace; an empty
// string is a host that flushed the VM.
func hostResponses(errs ...string) chan *miniclient.Response {
	resp := &miniclient.Response{}

	for i, err := range errs {
		resp.Resp = append(resp.Resp, &minicli.Response{
			Host:  "host" + strconv.Itoa(i+1),
			Error: err,
		})
	}

	out := make(chan *miniclient.Response, 1)
	out <- resp
	close(out)

	return out
}

// TestFlushErrors covers the answers of `vm flush <vm>`, which KillVM and
// RedeployVM send so that only the named VM is flushed and the other QUIT VMs
// of the namespace are kept.
func TestFlushErrors(t *testing.T) {
	tests := []struct {
		name    string
		answers []string
		want    string
	}{
		{name: "single host", answers: []string{""}},
		{name: "other hosts do not run the VM", answers: []string{"vm not found: h1", "", "vm not found: h1"}},
		{name: "no host runs the VM", answers: []string{"vm not found: h1", "vm not found: h1"}, want: "vm not found: h1"},
		{name: "a host fails", answers: []string{"", "clogged vm 4"}, want: "clogged vm 4"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := flushErrors(hostResponses(test.answers...))

			switch {
			case test.want == "" && err != nil:
				t.Errorf("flushErrors returned %v, want nil", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Errorf("flushErrors returned %v, want an error with %q", err, test.want)
			}
		})
	}
}

// TestFlushNeedsAVMName checks that flush never falls back to a bare
// `vm flush`, which would delete every QUIT VM of the namespace.
func TestFlushNeedsAVMName(t *testing.T) {
	if err := flush("exp", ""); err == nil {
		t.Error("flush without a VM name returned nil, want an error")
	}
}

// registerVMTagPatterns registers minimega's `namespace` and `vm tag`
// patterns (cmd/minimega/namespace_cli.go, vm_cli.go) with minicli for the
// rest of the test.
func registerVMTagPatterns(t *testing.T) {
	t.Helper()

	minicli.Reset()
	t.Cleanup(minicli.Reset)

	noop := func(*minicli.Command, chan<- minicli.Responses) {}

	minicli.MustRegister(&minicli.Handler{Patterns: []string{"namespace <name> (command)"}, Call: noop})
	minicli.MustRegister(&minicli.Handler{
		Patterns: []string{"vm tag <vm target> [key or all]", "vm tag <vm target> <key> <value>"},
		Call:     noop,
	})
}

// compileNamespaced parses cmd as minimega does when phenix sends it to a
// namespace: it compiles `namespace "exp" <cmd>`, then compiles the nested
// command again from the Original that minicli rebuilt from the parsed
// arguments, as namespaceCommands does with MustCompile before running it
// (cmd/minimega/cli.go). An error here makes minimega exit.
func compileNamespaced(cmd string) (*minicli.Command, error) {
	outer, err := minicli.Compile(`namespace "exp" ` + cmd)
	if err != nil {
		return nil, fmt.Errorf("compiling %q: %w", cmd, err)
	}

	if outer == nil || outer.Subcommand == nil {
		return nil, fmt.Errorf("compiling %q: no nested command", cmd)
	}

	inner, err := minicli.Compile(outer.Subcommand.Original)
	if err != nil {
		return nil, fmt.Errorf("compiling the rebuilt %q: %w", outer.Subcommand.Original, err)
	}

	return inner, nil
}

// vmTagKeyValue returns the key and value a compiled `vm tag` sets, and
// whether it sets one at all rather than reading the VM's tags.
func vmTagKeyValue(cmd *minicli.Command) (string, string, bool) {
	if cmd == nil {
		return "", "", false
	}

	value, ok := cmd.StringArgs["value"]

	return cmd.StringArgs["key"], value, ok
}

// TestVMTagCommandKeepsLabelsVerbatim is a regression test: SetVMTags quoted
// labels without escaping them, so minicli read `say "hi" twice` as
// `say hi twice`. Every label CheckVMTags accepts must reach the VM verbatim
// through both of minimega's compiles of the command; SetVMTags also turns
// minimega's preprocessing off (TestSetVMTagsSendsLabelsVerbatim).
func TestVMTagCommandKeepsLabelsVerbatim(t *testing.T) {
	registerVMTagPatterns(t)

	for _, label := range []string{
		"web",
		`say "hi" twice`,
		`C:\Program Files\minimega\`,
		"two\nlines",
		"tab\there",
		`it's fine`,
		"a #b",
		"é",
	} {
		if err := CheckVMTags("h2", map[string]string{label: label}); err != nil {
			t.Errorf("CheckVMTags refused %q: %v", label, err)

			continue
		}

		cmd, err := compileNamespaced(vmTagCommand("h2", label, label))
		if err != nil {
			t.Errorf("minimega cannot run the vm tag command for %q: %v", label, err)

			continue
		}

		if key, value, ok := vmTagKeyValue(cmd); cmd.StringArgs["vm"] != "h2" || !ok || key != label || value != label {
			t.Errorf("minimega read vm %q, key %q, value %q (set %v), want h2 and %q for both",
				cmd.StringArgs["vm"], key, value, ok, label)
		}
	}
}

// TestCheckVMTagsRefusesLabelsMinimegaCannotRebuild is a regression test: with
// labels escaped, a label that minicli cannot parse again once it rebuilt the
// namespaced command made minimega exit, or reached the VM changed or not at
// all. Each refused label is shown to break minimega's second compile.
func TestCheckVMTagsRefusesLabelsMinimegaCannotRebuild(t *testing.T) {
	registerVMTagPatterns(t)

	const noWhitespace = "holds a double quote, single quote, backslash or # but no whitespace"

	for _, tc := range []struct {
		label, reason string
	}{
		{`a"b`, noWhitespace},
		{`"`, noWhitespace},
		{"it's", noWhitespace},
		{`a\b`, noWhitespace},
		{`C:\minimega\`, noWhitespace},
		{"a#b", noWhitespace},
		{"", "is empty"},
		{"x \x01y", "holds the non-printable character"},
		{"a\u00a0b", "holds the non-printable character"},
		{"a b\vc", "holds the non-printable character"},
		{"\xff", "is not valid UTF-8"},
	} {
		cmd, err := compileNamespaced(vmTagCommand("h2", "note", tc.label))
		if _, value, ok := vmTagKeyValue(cmd); err == nil && ok && value == tc.label {
			t.Errorf("minimega reads %q verbatim, so CheckVMTags need not refuse it", tc.label)
		}

		err = CheckVMTags("h2", map[string]string{"role": "web", "note": tc.label})
		if !errors.Is(err, ErrInvalidVMLabel) {
			t.Errorf("CheckVMTags accepted the value %q: %v", tc.label, err)

			continue
		}

		for _, want := range []string{`label "note" of VM h2`, "its value", tc.reason} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("CheckVMTags error for the value %q = %q, want it to contain %q", tc.label, err, want)
			}
		}

		err = CheckVMTags("h2", map[string]string{tc.label: "v"})
		if !errors.Is(err, ErrInvalidVMLabel) || !strings.Contains(err.Error(), "its key") {
			t.Errorf("CheckVMTags for the key %q = %v, want ErrInvalidVMLabel naming its key", tc.label, err)
		}
	}
}

// TestSetVMTagsRefusesBeforeClearingTags checks that a label minimega cannot
// store fails SetVMTags before it sends `clear vm tag`, so the VM keeps its
// tags and minimega never sees the label.
func TestSetVMTagsRefusesBeforeClearingTags(t *testing.T) {
	withoutMinimega(t)

	err := Minimega{}.SetVMTags(NS("exp"), VMName("h2"), Tags(map[string]string{"role": "web", "note": `a"b`}))
	if !errors.Is(err, ErrInvalidVMLabel) || strings.Contains(err.Error(), "clear tags") {
		t.Errorf("SetVMTags with the label note=a\"b returned %v, want ErrInvalidVMLabel before clearing tags", err)
	}

	// The same call with valid labels reaches minimega, which is down here.
	err = Minimega{}.SetVMTags(NS("exp"), VMName("h2"), Tags(map[string]string{"role": "web"}))
	if err == nil || !strings.Contains(err.Error(), "failed to clear tags for vm h2") {
		t.Errorf("SetVMTags with valid labels and minimega down returned %v, want the clear vm tag failure", err)
	}
}

// TestDiskInjectCommands is a regression test: a redeploy that replicates
// injections recreated the disk snapshot from the image and replayed the
// node's injections but not its deletions, so files the topology deletes, from
// the image or from an injection, came back.
func TestDiskInjectCommands(t *testing.T) {
	const snap = "/tmp/minimega/files/host_exp_h1_snapshot"

	tests := []struct {
		name      string
		part      int
		injects   []string
		deletions []string
		want      []string
	}{
		{
			name:      "partition 0",
			injects:   []string{"/a:/b"},
			deletions: []string{"/c"},
		},
		{
			name:    "injections only",
			part:    1,
			injects: []string{"/a:/b", "/c:/d"},
			want:    []string{"disk inject " + snap + ":1 files /a:/b /c:/d"},
		},
		{
			name:      "deletions only",
			part:      2,
			deletions: []string{"/e2e-base-marker"},
			want:      []string{"disk inject " + snap + `:2 delete files "/e2e-base-marker"`},
		},
		{
			name:      "deletions after injections",
			part:      1,
			injects:   []string{"/a:/e2e-del-a"},
			deletions: []string{"/e2e-base-marker", "/e2e-del-a", `/say "hi"`},
			want: []string{
				"disk inject " + snap + ":1 files /a:/e2e-del-a",
				"disk inject " + snap + `:1 delete files "/e2e-base-marker","/e2e-del-a","/say \"hi\""`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := diskInjectCommands(snap, test.part, test.injects, test.deletions)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("diskInjectCommands = %q, want %q", got, test.want)
			}
		})
	}
}

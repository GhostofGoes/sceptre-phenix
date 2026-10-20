package mm

import (
	"slices"
	"strings"
	"testing"

	"github.com/activeshadow/libminimega/minicli"
)

// TestSetVMTagsSendsLabelsVerbatim is a regression test: minimega expands $
// parameters in a `vm tag` with its own environment and rewrites file: and
// http(s): values into local paths before it stores them, so the label
// note=$HOME read /root and the label of a URL a path in its files directory.
func TestSetVMTagsSendsLabelsVerbatim(t *testing.T) {
	fake := &fakeMinimega{answer: func(string) minicli.Responses { return minicli.Responses{{Host: "head"}} }}
	fake.serve(t)

	err := Minimega{}.SetVMTags(NS("exp"), VMName("h2"), Tags(map[string]string{"note": "$HOME"}))
	if err != nil {
		t.Fatalf("SetVMTags: %v", err)
	}

	want := []string{
		`.preprocess false namespace "exp" clear vm tag h2 `,
		`.preprocess false namespace "exp" vm tag h2 "note" "$HOME"`,
	}

	if got := fake.sent(); !slices.Equal(got, want) {
		t.Errorf("SetVMTags sent %q, want %q", got, want)
	}
}

// TestExecC2CommandSendsCommandsVerbatim is a regression test: minimega
// expanded $ parameters in a `cc exec` with its own environment, so the SOH
// Windows listener check ran `=Get-NetTCPConnection ...` instead of
// `$tcp=Get-NetTCPConnection ...`.
func TestExecC2CommandSendsCommandsVerbatim(t *testing.T) {
	fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
		if strings.Contains(cmd, "cc exec") {
			return minicli.Responses{{Host: "head", Data: "7"}}
		}

		return minicli.Responses{{Host: "head"}}
	}}
	fake.serve(t)

	const command = `powershell -command "$tcp=Get-NetTCPConnection; @($tcp)"`

	id, err := Minimega{}.ExecC2Command(C2NS("exp"), C2VM("h2"), C2Command(command), C2SkipActiveClientCheck(true))
	if err != nil || id != "7" {
		t.Fatalf("ExecC2Command = %q, %v; want command 7", id, err)
	}

	want := []string{
		`namespace "exp" cc filter name=h2`,
		`.preprocess false namespace "exp" cc exec ` + command,
		`namespace "exp" clear cc filter`,
	}

	if got := fake.sent(); !slices.Equal(got, want) {
		t.Errorf("ExecC2Command sent %q, want %q", got, want)
	}
}

// TestExecC2CommandClearsAFilterThatFailed is a regression test: minimega
// sends `cc filter` to every namespace host, and when one of them failed to
// answer, ExecC2Command returned before it registered the `clear cc filter`,
// so the hosts that applied the filter sent every later `cc` command of the
// namespace only to this VM.
func TestExecC2CommandClearsAFilterThatFailed(t *testing.T) {
	fake := &fakeMinimega{answer: func(cmd string) minicli.Responses {
		if strings.Contains(cmd, "cc filter name=") {
			return minicli.Responses{{Host: "head"}, {Host: "peer", Error: "timeout"}}
		}

		return minicli.Responses{{Host: "head"}}
	}}
	fake.serve(t)

	if _, err := (Minimega{}).ExecC2Command(C2NS("exp"), C2VM("h2"), C2Command("true"), C2SkipActiveClientCheck(true)); err == nil {
		t.Fatal("ExecC2Command succeeded, want the filter's error")
	}

	want := []string{`namespace "exp" cc filter name=h2`, `namespace "exp" clear cc filter`}
	if got := fake.sent(); !slices.Equal(got, want) {
		t.Errorf("ExecC2Command sent %q, want %q", got, want)
	}
}

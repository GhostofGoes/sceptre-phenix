package mmcli

import "testing"

// TestCommandStringNoPreprocess checks where `.preprocess false` goes: around
// the namespace, so minimega turns preprocessing off for the nested command
// and for the copies of it that it runs on each namespace host.
func TestCommandStringNoPreprocess(t *testing.T) {
	cmd := &Command{Command: `vm tag h1 "note" "$HOME"`, Namespace: "exp", NoPreprocess: true}

	want := `.record false .preprocess false namespace "exp" vm tag h1 "note" "$HOME"`
	if got := cmd.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	cmd.NoPreprocess = false

	want = `.record false namespace "exp" vm tag h1 "note" "$HOME"`
	if got := cmd.String(); got != want {
		t.Errorf("String() without NoPreprocess = %q, want %q", got, want)
	}
}

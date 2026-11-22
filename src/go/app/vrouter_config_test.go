package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"

	"phenix/util/testenv"
)

// vrouterConfigWords splits a line the way a shell does when nothing in it is
// special: on spaces, with single quotes removed.
func vrouterConfigWords(line string) []string {
	var (
		words  []string
		word   strings.Builder
		open   bool
		quoted bool
	)

	for _, r := range line {
		switch {
		case r == '\'':
			quoted = !quoted
			open = true
		case r == ' ' && !quoted:
			if open {
				words = append(words, word.String())
				word.Reset()

				open = false
			}
		default:
			word.WriteRune(r)

			open = true
		}
	}

	if open {
		words = append(words, word.String())
	}

	return words
}

// vrouterConfigSpecial holds words a shell acts on: file name patterns,
// expansions, second commands, redirections, comments, and quoting.
func vrouterConfigSpecial() []string {
	return []string{
		"[ab]", "[!x]", "a*", "?", "{a,b}", "~", "$HOME", "$(echo a)", "`echo a`",
		"a;b", "a|b", "a&b", ">x", "<a", "#a", "!!", `\a`, `"a"`, "'", "\n", "\t",
	}
}

// genVrouterConfigLine draws lines that mostly start as a VyOS command. Their
// words are plain, single-quoted, or one of vrouterConfigSpecial.
func genVrouterConfigLine() *rapid.Generator[string] {
	plain := []string{"a", "b1", "ab1", "10.0.0.1/24", "a=b", "x-y_z", "é", "", "", " "}
	special := vrouterConfigSpecial()

	return rapid.Custom(func(t *rapid.T) string {
		count := rapid.IntRange(0, 5).Draw(t, "words")
		words := make([]string, 0, count+1)
		words = append(words, rapid.SampledFrom([]string{"set", "set", "delete", "commit", ""}).Draw(t, "verb"))

		for range count {
			word := rapid.SampledFrom(plain).Draw(t, "plain")

			switch rapid.IntRange(0, 5).Draw(t, "kind") {
			case 0:
				word = rapid.SampledFrom(special).Draw(t, "special")
			case 1:
				word = "'" + rapid.SampledFrom(special[:len(special)-3]).Draw(t, "quoted") + "'"
			case 2:
				word += rapid.SampledFrom(special).Draw(t, "suffix")
			}

			words = append(words, word)
		}

		return strings.Join(words, " ")
	})
}

// vrouterConfigBashDir makes a directory with files that a file name pattern
// in a test line would match.
func vrouterConfigBashDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range []string{"a", "b", "1", "ab1"} {
		err := os.WriteFile(filepath.Join(dir, name), nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// vrouterConfigBashDiff runs a line in bash as source text, which is what it
// is in the rendered script, and compares the words bash gives the command
// with the words of the line. It returns the difference as (-want +got).
func vrouterConfigBashDiff(dir, line string) (string, error) {
	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", `args() { printf '%s\0' "$@"; }; args `+line)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bash on line %q: %w", line, err)
	}

	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")

	return cmp.Diff(vrouterConfigWords(line), got), nil
}

// TestCheckVyosConfigLineSpecialWords puts each word a shell acts on into a
// line, alone, between words, joined to a word, and in single quotes. bash
// must read every line checkVyosConfigLine accepts as one command whose words
// are the words of the line.
func TestCheckVyosConfigLineSpecialWords(t *testing.T) {
	t.Parallel()
	testenv.RequireCommands(t, "bash")

	dir := vrouterConfigBashDir(t)

	for _, word := range vrouterConfigSpecial() {
		lines := []string{"set " + word, "set a " + word + " b", "set a" + word, "delete " + word + "b"}
		if !strings.Contains(word, "'") {
			lines = append(lines, "set '"+word+"'", "set a'"+word+"'b")
		}

		for _, line := range lines {
			err := checkVyosConfigLine(line)
			if err != nil {
				if !errors.Is(err, ErrVyosConfigLine) {
					t.Errorf("checkVyosConfigLine(%q) = %v, want ErrVyosConfigLine", line, err)
				}

				continue
			}

			diff, err := vrouterConfigBashDiff(dir, line)
			if err != nil {
				t.Errorf("accepted line: %v", err)
			} else if diff != "" {
				t.Errorf("bash words of accepted line %q (-want +got):\n%s", line, diff)
			}
		}
	}
}

// TestCheckVyosConfigLineMatchesBash checks the same for lines that mix
// several such words.
func TestCheckVyosConfigLineMatchesBash(t *testing.T) {
	t.Parallel()
	testenv.RequireCommands(t, "bash")

	dir := vrouterConfigBashDir(t)

	rapid.Check(t, func(t *rapid.T) {
		line := genVrouterConfigLine().Draw(t, "line")

		err := checkVyosConfigLine(line)
		if err != nil {
			if !errors.Is(err, ErrVyosConfigLine) {
				t.Fatalf("checkVyosConfigLine(%q) = %v, want ErrVyosConfigLine", line, err)
			}

			return
		}

		diff, err := vrouterConfigBashDiff(dir, line)
		if err != nil {
			t.Fatalf("accepted line: %v", err)
		}

		if diff != "" {
			t.Fatalf("bash words of accepted line %q (-want +got):\n%s", line, diff)
		}
	})
}

// TestCheckVyosConfigLine checks lines a router configuration needs, and
// lines that would run something else.
func TestCheckVyosConfigLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		ok   bool
	}{
		{name: "set", line: "set protocols bgp system-as 64505", ok: true},
		{name: "delete", line: "delete service ntp", ok: true},
		{name: "quoted regex", line: "set policy as-path-list T rule 10 regex '^64500_(64501|64502)$'", ok: true},
		{name: "quoted text", line: "set interfaces dummy dum0 description 'a; b & c'", ok: true},
		{name: "empty quoted word", line: "set system login banner pre-login ''", ok: true},
		{name: "address and prefix", line: "set interfaces vti vti101 address 10.255.0.1/31", ok: true},
		{name: "empty", line: ""},
		{name: "verb alone", line: "set"},
		{name: "leading space", line: " set system host-name a"},
		{name: "other command", line: "commit"},
		{name: "second command", line: "set system host-name a; reboot"},
		{name: "background", line: "set system host-name a & reboot"},
		{name: "pipe", line: "set system host-name a | tee /tmp/x"},
		{name: "redirect", line: "set system host-name a > /etc/passwd"},
		{name: "variable", line: "set system host-name $HOSTNAME"},
		{name: "command substitution", line: "set system host-name $(hostname)"},
		{name: "backtick", line: "set system host-name `hostname`"},
		{name: "backslash", line: `set system host-name a\ b`},
		{name: "double quote", line: `set system host-name "a"`},
		{name: "comment", line: "set system host-name a #b"},
		{name: "star", line: "set system host-name *"},
		{name: "question mark", line: "set system host-name ?"},
		{name: "bracket pattern", line: "set system host-name [ab]"},
		{name: "brace", line: "set system host-name {a,b}"},
		{name: "tilde", line: "set system host-name ~root"},
		{name: "history", line: "set system host-name !!"},
		{name: "newline", line: "set system host-name a\nreboot"},
		{name: "carriage return", line: "set system host-name a\rreboot"},
		{name: "tab", line: "set system host-name\ta"},
		{name: "unclosed quote", line: "set system login banner pre-login 'hello"},
		{name: "quote opens after text", line: "set system host-name a'; reboot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkVyosConfigLine(tt.line)
			if tt.ok && err != nil {
				t.Errorf("checkVyosConfigLine(%q) = %v, want nil", tt.line, err)
			}

			if !tt.ok && !errors.Is(err, ErrVyosConfigLine) {
				t.Errorf("checkVyosConfigLine(%q) = %v, want ErrVyosConfigLine", tt.line, err)
			}
		})
	}
}

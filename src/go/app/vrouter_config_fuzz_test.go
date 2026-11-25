package app

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// vrouterConfigQuoted matches a single-quoted part of a line.
var vrouterConfigQuoted = regexp.MustCompile(`'[^']*'`)

// vrouterConfigShellChars is the model's own list of the characters the shell
// acts on outside single quotes, kept apart from vyosConfigUnsafe so that the
// fuzz test does not check the list against itself.
const vrouterConfigShellChars = "\"`$;|&<>(){}[]\\#!*?~"

// FuzzCheckVyosConfigLine checks checkVyosConfigLine against a model of a safe
// line: it starts with set or delete and a space, the next character is a
// lowercase letter, it has no control character, and what is left when the
// single-quoted parts are taken out has no quote and nothing the shell acts
// on. A refused line gives ErrVyosConfigLine.
func FuzzCheckVyosConfigLine(f *testing.F) {
	for _, seed := range []string{
		"set protocols bgp system-as 64505",
		"delete service ntp",
		"set policy as-path-list T rule 10 regex '^64500_(64501|64502)$'",
		"set system login banner pre-login ''",
		"",
		"commit",
		"set system host-name a; reboot",
		"set system host-name $(hostname)",
		"set system host-name `hostname`",
		"set system host-name [ab]",
		"set system host-name a\nreboot",
		"set system login banner pre-login 'hello",
		"set system host-name a'; reboot",
		"set system host-name \xff",
		"set -xk",
		"set '-n'",
		"delete -x",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		bare := vrouterConfigQuoted.ReplaceAllString(line, "")
		rest, verb := strings.CutPrefix(line, "set ")
		if !verb {
			rest, verb = strings.CutPrefix(line, "delete ")
		}

		safe := verb && rest != "" && rest[0] >= 'a' && rest[0] <= 'z' &&
			!strings.ContainsFunc(line, unicode.IsControl) &&
			!strings.ContainsAny(bare, "'"+vrouterConfigShellChars)

		err := checkVyosConfigLine(line)
		if safe && err != nil {
			t.Fatalf("checkVyosConfigLine(%q) = %v, want nil", line, err)
		}

		if !safe && !errors.Is(err, ErrVyosConfigLine) {
			t.Fatalf("checkVyosConfigLine(%q) = %v, want ErrVyosConfigLine", line, err)
		}
	})
}

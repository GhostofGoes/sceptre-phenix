package app

import (
	"errors"
	"strings"
	"testing"
	"unicode"
)

// FuzzBGPLinesText checks the texts that the VyOS renderer of the `bgp`
// metadata writes in single quotes: the description and the password of a
// neighbor, and the regex of a community list rule. The model of a safe text
// is one with no single quote and no control character. A block with a text
// that is not safe is refused with ErrBGPConfig. Every line of an accepted
// block is a line the `config` list would take, and has its text in single
// quotes at its end.
func FuzzBGPLinesText(f *testing.F) {
	for _, seed := range []string{
		"ShadeCast Cable",
		"_64500:1_",
		"^(64500|64501):[0-9]+$",
		"s3cret",
		"",
		"it's",
		"'a'",
		"a'; reboot; '",
		"$(reboot)",
		"`reboot`",
		"a\nreboot",
		"a\tb",
		"a\x00b",
		"\x7f",
		"\u0085",
		"é",
		"\xff",
		"-x",
		"a b",
	} {
		f.Add(seed, seed, seed)
	}

	f.Fuzz(func(t *testing.T, description, password, regex string) {
		unsafe := func(text string) bool {
			return strings.ContainsFunc(text, func(r rune) bool { return r == '\'' || unicode.IsControl(r) })
		}

		lines, err := bgpLines(bgpTextMetadata(description, password, regex), osTypeVyos, nil, nil)
		if err != nil {
			if !errors.Is(err, ErrBGPConfig) {
				t.Fatalf("bgpLines() error = %v, want ErrBGPConfig", err)
			}

			return
		}

		if unsafe(description) || unsafe(password) || unsafe(regex) {
			t.Fatalf("bgpLines() took the texts %q, %q, %q, want ErrBGPConfig", description, password, regex)
		}

		for _, line := range lines {
			if err := checkVyosConfigLine(line); err != nil {
				t.Fatalf("checkVyosConfigLine(%q) = %v, want nil", line, err)
			}
		}

		want := []string{regex, description, password}
		if description == "" {
			want = []string{regex, password}
		}

		found := bgpTextLines(lines)
		if len(found) != len(want) {
			t.Fatalf("bgpLines() gave %d lines with a text, want %d: %q", len(found), len(want), found)
		}

		for i, text := range want {
			if !strings.HasSuffix(found[i], " '"+text+"'") {
				t.Fatalf("line %q does not end with the text %q in single quotes", found[i], text)
			}
		}
	})
}

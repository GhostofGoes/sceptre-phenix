package mmcli

import "testing"

func TestEscape(t *testing.T) {
	tests := map[string]string{
		"web server":       "web server",
		`say "hi"`:         `say \"hi\"`,
		`C:\minimega`:      `C:\\minimega`,
		"line one\nline 2": `line one\nline 2`,
		"tab\there":        `tab\there`,
		"#not a comment":   "#not a comment",
	}

	for in, want := range tests {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

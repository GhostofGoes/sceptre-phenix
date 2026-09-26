package broker

import "testing"

func TestDelayLess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want bool
	}{
		{"timer:30s", "timer:5m", true},
		{"timer:5m", "timer:30s", false},
		{"", "timer:30s", true},
		{"cc:a", "user", true},
	}

	for _, c := range cases {
		if got := delayLess(c.a, c.b); got != c.want {
			t.Errorf("delayLess(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

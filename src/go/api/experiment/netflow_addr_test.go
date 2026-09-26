package experiment

import "testing"

func TestSplitAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		addr, host, port string
		ok               bool
	}{
		{"10.0.0.1:443", "10.0.0.1", "443", true},
		{"fe80::1:22", "fe80::1", "22", true},
		{"garbage", "", "", false},
	}

	for _, c := range cases {
		host, port, ok := splitAddr(c.addr)
		if host != c.host || port != c.port || ok != c.ok {
			t.Errorf("splitAddr(%q) = %q, %q, %v", c.addr, host, port, ok)
		}
	}
}

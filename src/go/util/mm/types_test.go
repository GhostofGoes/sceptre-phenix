package mm

import (
	"math"
	"slices"
	"testing"
)

// TestPaginate is a regression test: a page or size below 1, or a product of
// the two that overflowed, gave Paginate a negative slice bound, which
// panicked; a websocket VM list request reaches it with values a client sends.
func TestPaginate(t *testing.T) {
	vms := VMs{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	names := func(v VMs) []string {
		n := make([]string, 0, len(v))
		for _, vm := range v {
			n = append(n, vm.Name)
		}

		return n
	}

	for _, tc := range []struct {
		page, size int
		want       []string
	}{
		{1, 2, []string{"a", "b"}},
		{2, 2, []string{"c"}},
		{3, 2, []string{}},
		{1, 10, []string{"a", "b", "c"}},
		{1, math.MaxInt, []string{"a", "b", "c"}},
		{math.MaxInt, 2, []string{}},
		{math.MaxInt / 2, 4, []string{}},
		{0, 2, []string{}},
		{-1, 2, []string{}},
		{1, 0, []string{}},
		{1, -5, []string{}},
	} {
		if got := names(vms.Paginate(tc.page, tc.size)); !slices.Equal(got, tc.want) {
			t.Errorf("Paginate(%d, %d) = %v, want %v", tc.page, tc.size, got, tc.want)
		}
	}
}

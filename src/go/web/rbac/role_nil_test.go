package rbac

import "testing"

func TestZeroRoleAllowsNothing(t *testing.T) {
	var r Role

	if r.Allowed("experiments", "list") {
		t.Error("zero role allowed experiments list")
	}
}

package scorchmd

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRunNamesUndefinedComponents(t *testing.T) {
	md := ScorchMetadata{
		Components: []ComponentSpec{{Name: "wait1", Type: "pause"}, {Name: "echo"}},
		Runs: []*Loop{
			{Start: []string{"wait1"}, Stop: []string{"echo"}, Loop: &Loop{Start: []string{"echo"}}},
			{Start: []string{"wait1", "nosuchcmp"}},
			{Start: []string{"wait1"}, Loop: &Loop{Loop: &Loop{Cleanup: []string{"typo"}}}},
		},
	}

	tests := []struct {
		run  int
		want string // empty: valid
	}{
		{0, ""},
		{1, `scorch run 1 start stage names component "nosuchcmp"`},
		{2, `scorch run 2 loop 2 cleanup stage names component "typo"`},
	}

	for _, tt := range tests {
		err := md.ValidateRun(tt.run)

		switch {
		case tt.want == "" && err != nil:
			t.Errorf("ValidateRun(%d) = %v, want nil", tt.run, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("ValidateRun(%d) = %v, want an error containing %q", tt.run, err, tt.want)
		case tt.want != "" && !errors.Is(err, ErrUndefinedComponent):
			t.Errorf("ValidateRun(%d) = %v, want it to wrap ErrUndefinedComponent", tt.run, err)
		}
	}

	if err := md.Validate(); err == nil || !strings.Contains(err.Error(), "nosuchcmp") {
		t.Errorf("Validate() = %v, want the first undefined component, nosuchcmp", err)
	}
}

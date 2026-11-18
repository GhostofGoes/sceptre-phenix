package scorchexe

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hashicorp/go-multierror"
)

// nest collects errs the way a run does: in a loop's errors, in the run's.
func nest(errs ...error) error {
	loop := fmt.Errorf("executing Scorch for run 0, count 0: %w", multierror.Append(nil, errs...))

	return fmt.Errorf("running Scorch for experiment exp: %w", multierror.Append(nil, loop))
}

func TestCanceled(t *testing.T) {
	var (
		stopFailed = errors.New("stop component qos1: exit status 1")
		canceled   = fmt.Errorf("start component wait120: %w", context.Canceled)
		killed     = fmt.Errorf("start component cc1: signal: terminated (%w)", context.Canceled)
	)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, true},
		{"wrapped", canceled, true},
		{"other", stopFailed, false},
		{"deadline", context.DeadlineExceeded, false},
		{"all canceled", multierror.Append(nil, canceled, killed), true},
		{"teardown failed", multierror.Append(nil, canceled, stopFailed), false},
		{"joined", errors.Join(canceled, stopFailed), false},
		{"nested", nest(canceled), true},
		{"nested teardown failure", nest(canceled, stopFailed), false},
		{"empty", &multierror.Error{}, false},
	}

	for _, tt := range tests {
		if got := Canceled(tt.err); got != tt.want {
			t.Errorf("%s: Canceled(%v) = %t, want %t", tt.name, tt.err, got, tt.want)
		}
	}
}

package scorchexe

import (
	"context"
	"errors"

	"github.com/hashicorp/go-multierror"
)

// Canceled reports whether err records only that a run was canceled: every
// error it collects is, or wraps, [context.Canceled]. The error of a canceled
// run whose stop or cleanup stage then failed is not.
func Canceled(err error) bool {
	for {
		switch e := err.(type) { //nolint:errorlint // walks the tree of collected errors
		case nil:
			return false
		case *multierror.Error:
			return allCanceled(e.Errors)
		case interface{ Unwrap() []error }:
			return allCanceled(e.Unwrap())
		}

		if !errors.Is(err, context.Canceled) {
			return false
		}

		next := errors.Unwrap(err)
		if next == nil {
			return true
		}

		err = next
	}
}

func allCanceled(errs []error) bool {
	if len(errs) == 0 {
		return false
	}

	for _, err := range errs {
		if !Canceled(err) {
			return false
		}
	}

	return true
}

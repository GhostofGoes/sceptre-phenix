package web

import (
	"errors"
	"net/http"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/store"
	"phenix/util/mm"
	"phenix/web/util"
)

// vmErrorStatus returns the HTTP status that answers err, the error of an
// operation on a VM: 409 for a VM that is not running, 404 for an experiment
// the store does not hold, 400 for a request phenix refuses, such as one for
// a stopped experiment, an unknown VM, a disk phenix will not give a VM, an
// unsafe file name or the name of an existing image, and 500 for anything
// else, such as minimega or the store failing.
func vmErrorStatus(err error) int {
	switch {
	case errors.Is(err, mm.ErrVMNotRunning):
		return http.StatusConflict
	case errors.Is(err, store.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, experiment.ErrExperimentNotRunning),
		errors.Is(err, mm.ErrVMNotFound),
		errors.Is(err, mm.ErrScreenshotNotFound),
		errors.Is(err, mm.ErrUnsafeArgument),
		errors.Is(err, mm.ErrInvalidVMLabel),
		errors.Is(err, vm.ErrInvalidDisk),
		errors.Is(err, mm.ErrInvalidFileName),
		errors.Is(err, vm.ErrImageExists),
		errors.Is(err, util.ErrInvalidScreenshotSize):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

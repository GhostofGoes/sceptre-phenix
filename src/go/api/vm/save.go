package vm

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"phenix/util/mm/mmcli"
)

//nolint:gochecknoglobals // overridden by tests
var (
	// vmSaveStatusTimeout bounds how long Snapshot keeps polling while QEMU
	// reports no migration for a `vm save` minimega accepted.
	vmSaveStatusTimeout = time.Minute

	// vmSaveTimeout bounds a whole save. QEMU keeps copying the memory of a
	// guest that dirties it faster than it is copied, so such a save never
	// completes on its own.
	vmSaveTimeout = 30 * time.Minute

	// saveCommandTimeout bounds each command of a save that minimega answers
	// only once QEMU answered it over QMP, which minimega waits for without a
	// deadline: the polls of the save, and the cancel and resume of a failed
	// one.
	saveCommandTimeout = time.Minute
)

// migrationDone are the statuses of a QEMU migration that is not running, so
// there is nothing to cancel; "" is QEMU's answer when there was none.
//
//nolint:gochecknoglobals // constant list
var migrationDone = []string{"", "none", statusCompleted, statusFailed, statusCancelled, "cancelling"}

// migration is the part of QEMU's query-migrate answer a save needs.
type migration struct {
	Status string `json:"status"`
	RAM    struct {
		Total       float64 `json:"total"`
		Transferred float64 `json:"transferred"`
	} `json:"ram"`
}

// queryMigration returns the status of the migration QEMU runs for vmName,
// which `vm save` started to save its memory. It asks the VM's own QEMU over
// QMP: minimega's `vm save` status table asks every KVM VM of the namespace on
// the host, so one powered-off VM fails it for all, and minimega panics on any
// status other than completed, failed or active, such as QEMU's setup and
// cancelled.
func queryMigration(expName, vmName string) (migration, error) {
	cmd := mmcli.NewNamespacedCommand(expName)

	// The JSON keeps its spaces: minimega rebuilds a namespaced command quoting
	// only the arguments that hold whitespace, and parses it again.
	cmd.Command = fmt.Sprintf(`vm qmp %s '{ "execute": "query-migrate" }'`, vmName)
	cmd.Timeout = saveCommandTimeout

	resp, err := mmcli.SingleResponse(mmcli.Run(cmd))
	if err != nil {
		return migration{}, fmt.Errorf("querying the memory snapshot of VM %s: %w", vmName, err)
	}

	var answer struct {
		Return *migration `json:"return"`
	}

	if err := json.Unmarshal([]byte(resp), &answer); err != nil || answer.Return == nil {
		return migration{}, fmt.Errorf("querying the memory snapshot of VM %s: QEMU answered %q", vmName, resp)
	}

	return *answer.Return, nil
}

// waitForVMSave polls the migration that saves the memory of vmName until
// QEMU reports it completed, and passes its progress in percent to cb. A
// failed or cancelled save, a failed poll, no migration for statusTimeout in
// a row and a save still running after timeout are returned as errors.
func waitForVMSave(
	vmName string,
	poll func() (migration, error),
	cb func(string),
	interval, statusTimeout, timeout time.Duration,
) error {
	var (
		idle  time.Duration
		start = time.Now()
	)

	for {
		status, err := poll()
		if err != nil {
			return err
		}

		switch status.Status {
		case "", "none":
			if idle >= statusTimeout {
				return fmt.Errorf("QEMU reported no memory snapshot for VM %s for %v", vmName, statusTimeout)
			}

			idle += interval
		case statusCompleted:
			if cb != nil {
				cb(statusCompleted)
			}

			return nil
		case statusFailed, statusCancelled:
			return fmt.Errorf("memory snapshot for VM %s %s", vmName, status.Status)
		default:
			idle = 0

			if cb != nil && status.RAM.Total > 0 {
				cb(fmt.Sprintf("%f", status.RAM.Transferred/status.RAM.Total*100)) //nolint:mnd // percent
			}
		}

		// The polls take time too, so the time that passed is measured.
		if time.Since(start) >= timeout {
			return fmt.Errorf("memory snapshot for VM %s did not complete within %v", vmName, timeout)
		}

		time.Sleep(interval)
	}
}

// migrationRunning reports whether QEMU still runs the migration of status.
func migrationRunning(status migration) bool {
	return !slices.Contains(migrationDone, status.Status)
}

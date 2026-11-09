package scorchexe

import "time"

// TeardownTimeout bounds the stop and cleanup stages of a canceled run: from
// the cancel, they get this long in all, at every loop level. An experiment
// stop waits longer than this for the runs it cancels, so their teardown ends
// before the stop kills the VMs and clears the minimega namespace.
const TeardownTimeout = 4 * time.Minute

package app

import "time"

// SetStopPollInterval sets how often a periodic app with a longer period
// checks whether its run ended, and returns a function that restores it.
func SetStopPollInterval(d time.Duration) func() {
	original := stopPollInterval
	stopPollInterval = d

	return func() { stopPollInterval = original }
}

// SetMinPeriod sets the shortest runPeriodically period phenix accepts, and
// returns a function that restores it.
func SetMinPeriod(d time.Duration) func() {
	original := minPeriod
	minPeriod = d

	return func() { minPeriod = original }
}

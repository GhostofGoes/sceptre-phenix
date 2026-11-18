package util

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/web/cache"
)

const (
	screenshotCacheDuration = 10 * time.Second

	// MaxScreenshotSize bounds the size of a screenshot, the largest dimension
	// in pixels minimega scales it to.
	MaxScreenshotSize = 4096
)

// ErrInvalidScreenshotSize is returned for a size that is not a whole number
// of pixels from 0, minimega's full size, to MaxScreenshotSize.
var ErrInvalidScreenshotSize = errors.New("invalid screenshot size")

// ScreenshotSize returns size in its canonical spelling, so equal sizes share a
// cache entry, or an error wrapping ErrInvalidScreenshotSize.
func ScreenshotSize(size string) (string, error) {
	n, err := strconv.Atoi(size)
	if err != nil || n < 0 || n > MaxScreenshotSize {
		return "", fmt.Errorf(
			"%w %q: want a whole number of pixels from 0 to %d",
			ErrInvalidScreenshotSize, size, MaxScreenshotSize,
		)
	}

	return strconv.Itoa(n), nil
}

// GetScreenshot returns a PNG screenshot of the VM scaled to size, from a
// cache that holds each size of each VM's screenshot separately for
// screenshotCacheDuration.
func GetScreenshot(expName, vmName, size string) ([]byte, error) {
	size, err := ScreenshotSize(size)
	if err != nil {
		return nil, err
	}

	// The websocket screenshot ticker would otherwise ask minimega during a
	// stop, after the stop cleared the namespace.
	if cache.IsExperimentStopping(expName) {
		return nil, fmt.Errorf("experiment %s is stopping: %w", expName, experiment.ErrExperimentNotRunning)
	}

	name := fmt.Sprintf("screenshot|%s/%s|%s", expName, vmName, size)

	if screenshot, ok := cache.Get(name); ok {
		return screenshot, nil
	}

	screenshot, err := vm.Screenshot(expName, vmName, size)
	if err != nil {
		return nil, fmt.Errorf("getting screenshot for VM: %w", err)
	}

	if screenshot == nil {
		return nil, errors.New("vm screenshot not found")
	}

	_ = cache.SetWithExpire(name, screenshot, screenshotCacheDuration)

	return screenshot, nil
}

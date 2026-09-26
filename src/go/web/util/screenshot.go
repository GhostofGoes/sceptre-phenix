package util

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"phenix/api/vm"
	"phenix/web/cache"
)

const (
	screenshotCacheDuration = 10 * time.Second
	// maxScreenshotSize bounds the size clients may ask for; it is passed to
	// minimega as part of a command.
	maxScreenshotSize = 4096
)

var ErrInvalidScreenshotSize = errors.New("screenshot size must be a whole number from 1 to 4096")

func GetScreenshot(expName, vmName, size string) ([]byte, error) {
	if n, err := strconv.Atoi(size); err != nil || n < 1 || n > maxScreenshotSize {
		return nil, ErrInvalidScreenshotSize
	}

	// clients ask for different sizes (table rows, tiles, VNC zoom)
	name := fmt.Sprintf("%s_%s_%s", expName, vmName, size)

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

package util

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"phenix/api/experiment"
	"phenix/api/vm"
	"phenix/util/firecracker"
	"phenix/web/cache"
)

const (
	// ScreenshotCacheDuration is how long a screenshot is reused. The websocket
	// screenshot ticker runs just longer, so clients whose rounds are offset in
	// time share screenshots.
	ScreenshotCacheDuration = 14 * time.Second
	// maxScreenshotSize bounds the size clients may ask for; it is passed to
	// minimega as part of a command.
	maxScreenshotSize = 4096
	// screenshotStep is what screenshot widths are rounded up to a multiple of.
	screenshotStep = 200
	// vmTypeCacheDuration is how long the VM types of an experiment's nodes are reused.
	vmTypeCacheDuration = 1 * time.Minute
)

var ErrInvalidScreenshotSize = errors.New("screenshot size must be a whole number from 1 to 4096")

// screenshotFlights merges concurrent requests for the same screenshot into one
// minimega command.
var screenshotFlights singleflight.Group //nolint:gochecknoglobals // shared by concurrent callers

// ValidScreenshotSize reports whether size is a whole number of pixels
// minimega can take a screenshot at.
func ValidScreenshotSize(size string) bool {
	n, err := strconv.Atoi(size)

	return err == nil && n >= 1 && n <= maxScreenshotSize
}

// ScreenshotWidth returns size rounded up to a multiple of 200 pixels, at most 4096, so the
// UI's tables, tiles and console zoom levels share a few cached widths.
func ScreenshotWidth(size string) (string, error) {
	if !ValidScreenshotSize(size) {
		return "", ErrInvalidScreenshotSize
	}

	n, _ := strconv.Atoi(size)

	return strconv.Itoa(min((n+screenshotStep-1)/screenshotStep*screenshotStep, maxScreenshotSize)), nil
}

// GetScreenshot returns a screenshot of a running VM. A firecracker VM has no
// screen, so for one it returns an error that wraps vm.ErrNotSupported and
// sends no command to minimega.
func GetScreenshot(expName, vmName, size string) ([]byte, error) {
	return GetScreenshotContext(context.Background(), expName, vmName, size)
}

// GetScreenshotContext is GetScreenshot at size's ScreenshotWidth, returning ctx's error once ctx
// is done while the shared minimega command runs on and caches its screenshot.
func GetScreenshotContext(ctx context.Context, expName, vmName, size string) ([]byte, error) {
	size, err := ScreenshotWidth(size)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s_%s_%s", expName, vmName, size)

	if screenshot, ok := cache.Get(name); ok {
		return screenshot, nil
	}

	if nodeVMType(expName, vmName) == firecracker.VMType {
		return nil, fmt.Errorf("getting screenshot for VM: %w", vm.NotSupportedError("taking a screenshot"))
	}

	flight := screenshotFlights.DoChan(name, func() (any, error) {
		// a flight that finished after the check above has cached the screenshot
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

		_ = cache.SetWithExpire(name, screenshot, ScreenshotCacheDuration)

		return screenshot, nil
	})

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for screenshot: %w", ctx.Err())
	case res := <-flight:
		if res.Err != nil {
			return nil, res.Err //nolint:wrapcheck // wrapped in the flight
		}

		screenshot, _ := res.Val.([]byte)

		return screenshot, nil
	}
}

// nodeVMType returns the minimega VM type of a topology node, or "" when the
// experiment does not have the node. It reads the experiment at most once a
// minute and caches the type of every node in it, because the screenshot
// callers ask for many VMs of one experiment in turn.
func nodeVMType(expName, vmName string) string {
	if vmType, ok := cache.Get(vmTypeCacheKey(expName, vmName)); ok {
		return string(vmType)
	}

	exp, err := experiment.Get(expName)
	if err != nil {
		return ""
	}

	var found string

	for _, node := range exp.Spec.Topology().Nodes() {
		vmType := vm.NodeVMType(node)

		_ = cache.SetWithExpire(
			vmTypeCacheKey(expName, node.General().Hostname()),
			[]byte(vmType),
			vmTypeCacheDuration,
		)

		if node.General().Hostname() == vmName {
			found = vmType
		}
	}

	return found
}

func vmTypeCacheKey(expName, vmName string) string {
	return "vm-type/" + expName + "/" + vmName
}

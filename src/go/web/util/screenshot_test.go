package util

import (
	"errors"
	"testing"
)

func TestGetScreenshotRejectsBadSizes(t *testing.T) {
	for _, size := range []string{"", "0", "-5", "4097", "200 file /etc/x", "1e3"} {
		if _, err := GetScreenshot("exp", "vm", size); !errors.Is(err, ErrInvalidScreenshotSize) {
			t.Errorf("size %q: err = %v, want ErrInvalidScreenshotSize", size, err)
		}
	}
}

package rbac

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrVCPULimitExceeded    = errors.New("requested vCPUs exceed role's resource limit")
	ErrMemoryLimitExceeded  = errors.New("requested memory exceeds role's resource limit")
	ErrDiskLimitExceeded    = errors.New("requested disk size exceeds role's resource limit")
	ErrVMCountLimitExceeded = errors.New("requested VM count exceeds role's resource limit")
)

// CheckVCPULimit returns an error if vcpus exceeds the role's MaxVCPUs limit.
// A limit of 0 (or no configured ResourceLimits) means unlimited.
func (r Role) CheckVCPULimit(vcpus int) error {
	limit := r.Spec.ResourceLimits

	if limit == nil || limit.MaxVCPUs == 0 {
		return nil
	}

	if vcpus > limit.MaxVCPUs {
		return fmt.Errorf("%w: requested %d, limit %d", ErrVCPULimitExceeded, vcpus, limit.MaxVCPUs)
	}

	return nil
}

// CheckMemoryLimit returns an error if memoryMB exceeds the role's
// MaxMemoryMB limit. A limit of 0 (or no configured ResourceLimits) means
// unlimited.
func (r Role) CheckMemoryLimit(memoryMB int) error {
	limit := r.Spec.ResourceLimits

	if limit == nil || limit.MaxMemoryMB == 0 {
		return nil
	}

	if memoryMB > limit.MaxMemoryMB {
		return fmt.Errorf(
			"%w: requested %dMB, limit %dMB",
			ErrMemoryLimitExceeded,
			memoryMB,
			limit.MaxMemoryMB,
		)
	}

	return nil
}

// CheckVMCountLimit returns an error if count exceeds the role's
// MaxVMsPerExperiment limit. A limit of 0 (or no configured ResourceLimits)
// means unlimited.
func (r Role) CheckVMCountLimit(count int) error {
	limit := r.Spec.ResourceLimits

	if limit == nil || limit.MaxVMsPerExperiment == 0 {
		return nil
	}

	if count > limit.MaxVMsPerExperiment {
		return fmt.Errorf(
			"%w: requested %d VMs, limit %d",
			ErrVMCountLimitExceeded,
			count,
			limit.MaxVMsPerExperiment,
		)
	}

	return nil
}

// CheckDiskSizeLimit returns an error if the given qemu-img style size string
// (e.g. "10G", "+5G", "1024M") exceeds the role's MaxDiskGB limit. A limit of
// 0 (or no configured ResourceLimits) means unlimited. Relative sizes
// (prefixed with '+' or '-') are checked against the requested delta itself,
// since the resulting absolute size can't be determined without inspecting
// the disk image; this is a conservative approximation.
func (r Role) CheckDiskSizeLimit(size string) error {
	limit := r.Spec.ResourceLimits

	if limit == nil || limit.MaxDiskGB == 0 {
		return nil
	}

	gb, err := parseSizeToGB(size)
	if err != nil {
		return fmt.Errorf("parsing disk size %q: %w", size, err)
	}

	if gb > float64(limit.MaxDiskGB) {
		return fmt.Errorf(
			"%w: requested %s (~%.2fGB), limit %dGB",
			ErrDiskLimitExceeded,
			size,
			gb,
			limit.MaxDiskGB,
		)
	}

	return nil
}

var sizeUnits = map[byte]float64{ //nolint:gochecknoglobals // lookup table
	'K': 1.0 / (1024 * 1024),
	'M': 1.0 / 1024,
	'G': 1,
	'T': 1024,
}

// parseSizeToGB parses a qemu-img style size string (e.g. "10G", "+5G",
// "1024M", "2T", or a plain byte count) into a size in GB.
func parseSizeToGB(size string) (float64, error) {
	s := strings.TrimSpace(size)
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "-")

	if s == "" {
		return 0, errors.New("empty size")
	}

	unit := unitToUpper(s[len(s)-1])

	factor, ok := sizeUnits[unit]
	if !ok {
		// no recognized unit suffix; assume raw bytes
		bytes, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size: %w", err)
		}

		return bytes / (1024 * 1024 * 1024), nil
	}

	value, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %w", err)
	}

	return value * factor, nil
}

func unitToUpper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}

	return b
}

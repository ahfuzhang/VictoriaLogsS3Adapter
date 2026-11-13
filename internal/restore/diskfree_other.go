//go:build !unix

package restore

import "errors"

// diskFreeBytes is not supported on this platform, so the free disk space check is skipped.
func diskFreeBytes(_ string) (uint64, error) {
	return 0, errors.ErrUnsupported
}

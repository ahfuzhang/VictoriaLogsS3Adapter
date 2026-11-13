//go:build unix

package restore

import "syscall"

// diskFreeBytes returns the number of bytes available to this process on the
// filesystem which holds path.
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	for {
		err := syscall.Statfs(path, &st)
		if err == nil {
			break
		}
		if err != syscall.EINTR {
			return 0, err
		}
	}
	//nolint:unconvert // the field types differ between platforms
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

//go:build unix

package hub

import "golang.org/x/sys/unix"

// diskFree reports the bytes available to the hub process on the file
// system holding dir.
func diskFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // field types differ per OS
}

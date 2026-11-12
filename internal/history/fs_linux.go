package history

import (
	"os"

	"golang.org/x/sys/unix"
)

// statFS classifies the file system that holds path by its statfs magic
// number, and a FUSE one by its mount's subtype.
func statFS(path string) (fsInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsInfo{}, err
	}
	return linuxFS(uint32(st.Type), path, readMountinfo), nil //nolint:gosec // G115: magic numbers are 32 bits, which some systems hold in a signed type.
}

// readMountinfo reads the system's table of this process's mounts.
func readMountinfo() ([]byte, error) {
	return os.ReadFile("/proc/self/mountinfo") //nolint:forbidigo // The kernel's table of mounts, not one of the store's files.
}

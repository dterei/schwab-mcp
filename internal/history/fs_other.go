//go:build !linux && !darwin && !windows

package history

// statFS knows no file system of this system, and takes every one for a
// local disk.
func statFS(string) (fsInfo, error) {
	return fsInfo{}, nil
}

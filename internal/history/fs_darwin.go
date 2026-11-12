package history

import "golang.org/x/sys/unix"

// statFS classifies the file system that holds path by its type name.
func statFS(path string) (fsInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsInfo{}, err
	}
	return darwinFS(unix.ByteSliceToString(st.Fstypename[:])), nil
}

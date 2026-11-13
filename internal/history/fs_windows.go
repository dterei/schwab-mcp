package history

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// statFS classifies the drive that holds path: a UNC path, or a drive that
// Windows says is remote, is on the network.
func statFS(path string) (fsInfo, error) {
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
	if err != nil {
		return fsInfo{}, err
	}
	return windowsFS(path, windows.GetDriveType(root)), nil
}

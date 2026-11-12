package history

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// fsInfo is what localDisk found out about the file system that holds the
// store.
type fsInfo struct {
	// network is a network file system, on which SQLite's WAL mode, which
	// needs shared memory and working locks, can damage the store.
	network bool
	// fuse is a FUSE file system that may be either.
	fuse bool
	// name is the file system's, as the system names it: nfs, fuse.encfs.
	name string
}

// Linux's statfs magic numbers of network file systems, and FUSE's.
var linuxNetwork = map[uint32]string{
	0x6969:     "nfs",
	0x517B:     "smb",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x5346414F: "afs",
	0x73757245: "coda",
	0x01021997: "9p", // WSL's /mnt/c, among others
}

const linuxFUSE = 0x65735546

// fuseNetwork are the FUSE file systems, by their subtype, that reach over
// a network.
var fuseNetwork = []string{"sshfs", "rclone", "s3fs", "gcsfuse", "davfs2", "smbnetfs", "curlftpfs"}

// linuxFS classifies a Linux file system by its statfs magic number. For
// FUSE it reads the mount's subtype from mountinfo, the contents of
// /proc/self/mountinfo, for the longest mount point that holds path.
func linuxFS(magic uint32, path string, mountinfo func() ([]byte, error)) fsInfo {
	if name, ok := linuxNetwork[magic]; ok {
		return fsInfo{network: true, name: name}
	}
	if magic != linuxFUSE {
		return fsInfo{}
	}
	fstype := "fuse"
	if b, err := mountinfo(); err == nil {
		if t := mountType(b, path); t != "" {
			fstype = t
		}
	}
	if sub, ok := strings.CutPrefix(fstype, "fuse."); ok && slices.Contains(fuseNetwork, sub) {
		return fsInfo{network: true, name: fstype}
	}
	return fsInfo{fuse: true, name: fstype}
}

// mountType returns the type of the file system mounted at the longest
// mount point in mountinfo that holds path, or "".
func mountType(mountinfo []byte, path string) string {
	best, fstype := -1, ""
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw
		before, after, ok := strings.Cut(sc.Text(), " - ")
		fields, types := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(types) < 1 {
			continue
		}
		mount := unescapeMount(fields[4])
		if len(mount) > best && within(path, mount) {
			best, fstype = len(mount), types[0]
		}
	}
	return fstype
}

// within reports whether path is dir or under it.
func within(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// unescapeMount undoes mountinfo's octal escapes of space, tab, newline and
// backslash.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// darwinFS classifies a macOS file system by its type name.
func darwinFS(name string) fsInfo {
	switch name {
	case "nfs", "smbfs", "afpfs", "webdav", "cifs":
		return fsInfo{network: true, name: name}
	case "macfuse", "osxfuse", "fuse-t":
		return fsInfo{fuse: true, name: name}
	}
	return fsInfo{}
}

// driveRemote is what Windows' GetDriveType returns for a network drive.
const driveRemote = 4

// windowsFS classifies a Windows path: a UNC path (\\server\share) or a
// drive of driveType DRIVE_REMOTE is on the network.
func windowsFS(path string, driveType uint32) fsInfo {
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`) {
		return fsInfo{network: true, name: "a network share"}
	}
	if driveType == driveRemote {
		return fsInfo{network: true, name: "a network drive"}
	}
	return fsInfo{}
}

// syncedNames are folders that a sync service keeps in step with a copy
// elsewhere. Each may be followed by a space and more, as in "Dropbox
// (Personal)" or "OneDrive - Contoso".
var syncedNames = []string{"Dropbox", "OneDrive", "Google Drive", "iCloudDrive"}

// syncedFolder returns the name of the sync service whose folder holds
// path, or "": a component named as one of syncedNames, or a path under
// Library/Mobile Documents (iCloud Drive) or Library/CloudStorage (the
// services that macOS's File Provider serves).
func syncedFolder(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	for i, p := range parts {
		for _, n := range syncedNames {
			if sameName(p, n) || len(p) > len(n) && sameName(p[:len(n)], n) && p[len(n)] == ' ' {
				return n
			}
		}
		if i+1 < len(parts) && sameName(p, "Library") {
			switch {
			case sameName(parts[i+1], "Mobile Documents"):
				return "iCloud Drive"
			case sameName(parts[i+1], "CloudStorage"):
				return "a cloud storage folder"
			}
		}
	}
	return ""
}

// localDisk finds out what kind of file system holds path: path itself, or
// when it does not exist yet, the nearest directory above it that does. It
// reads the file system by path, never with a descriptor of the store.
func localDisk(path string) (fsInfo, error) {
	p := path
	for {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fsInfo{}, err
		}
		up := filepath.Dir(p)
		if up == p {
			return fsInfo{}, err
		}
		p = up
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return statFS(p)
}

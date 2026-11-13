package history

import (
	"errors"
	"testing"
)

// The store's file system is classified, on fake statfs, mountinfo and
// drive-type answers: network file systems are refused, FUSE ones by their
// subtype, other FUSE ones warned of, and synced folders known by name.
func TestLocalDisk(t *testing.T) {
	t.Parallel()
	mountinfo := []byte(`22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 22 0:35 / /home/me/remote rw,nosuid shared:20 - fuse.sshfs me@host:/ rw,user_id=1000
41 40 0:36 / /home/me/remote/vault rw,nosuid shared:21 - fuse.encfs encfs rw
42 22 0:37 / /mnt/my\040drive rw shared:22 - fuse.rclone gdrive: rw
43 22 0:38 / /media/usb rw shared:23 - fuseblk /dev/sdb1 rw
44 22 0:39 / /home/me/remoteness rw shared:24 - fuse.encfs encfs rw
`)
	read := func() ([]byte, error) { return mountinfo, nil }
	linux := []struct {
		magic uint32
		path  string
		want  fsInfo
	}{
		{0xEF53, "/home/me/history.db", fsInfo{}}, // ext4
		{0x6969, "/home/me/history.db", fsInfo{network: true, name: "nfs"}},
		{0x517B, "/x", fsInfo{network: true, name: "smb"}},
		{0xFF534D42, "/x", fsInfo{network: true, name: "cifs"}},
		{0xFE534D42, "/x", fsInfo{network: true, name: "smb2"}},
		{0x5346414F, "/x", fsInfo{network: true, name: "afs"}},
		{0x73757245, "/x", fsInfo{network: true, name: "coda"}},
		{0x01021997, "/mnt/c/Users/me/history.db", fsInfo{network: true, name: "9p"}},
		{linuxFUSE, "/home/me/remote/history.db", fsInfo{network: true, name: "fuse.sshfs"}},
		{linuxFUSE, "/home/me/remote/vault/history.db", fsInfo{fuse: true, name: "fuse.encfs"}},
		{linuxFUSE, "/home/me/remoteness/history.db", fsInfo{fuse: true, name: "fuse.encfs"}},
		{linuxFUSE, "/mnt/my drive/history.db", fsInfo{network: true, name: "fuse.rclone"}},
		{linuxFUSE, "/media/usb/history.db", fsInfo{fuse: true, name: "fuseblk"}},
		{linuxFUSE, "/home/me/history.db", fsInfo{fuse: true, name: "ext4"}},
	}
	for _, c := range linux {
		if got := linuxFS(c.magic, c.path, read); got != c.want {
			t.Errorf("linuxFS(%#x, %s) = %+v, want %+v", c.magic, c.path, got, c.want)
		}
	}
	noMounts := func() ([]byte, error) { return nil, errors.New("no /proc") }
	if got, want := linuxFS(linuxFUSE, "/x", noMounts), (fsInfo{fuse: true, name: "fuse"}); got != want {
		t.Errorf("linuxFS of FUSE without mountinfo = %+v, want %+v", got, want)
	}
	for name, want := range map[string]fsInfo{
		"apfs":    {},
		"hfs":     {},
		"nfs":     {network: true, name: "nfs"},
		"smbfs":   {network: true, name: "smbfs"},
		"afpfs":   {network: true, name: "afpfs"},
		"webdav":  {network: true, name: "webdav"},
		"cifs":    {network: true, name: "cifs"},
		"macfuse": {fuse: true, name: "macfuse"},
		"osxfuse": {fuse: true, name: "osxfuse"},
		"fuse-t":  {fuse: true, name: "fuse-t"},
	} {
		if got := darwinFS(name); got != want {
			t.Errorf("darwinFS(%s) = %+v, want %+v", name, got, want)
		}
	}
	const driveFixed = 3
	for _, c := range []struct {
		path  string
		drive uint32
		want  fsInfo
	}{
		{`C:\Users\me\AppData\Local\schwabtx\history.db`, driveFixed, fsInfo{}},
		{`Z:\schwabtx\history.db`, driveRemote, fsInfo{network: true, name: "a network drive"}},
		{`\\server\share\history.db`, 0, fsInfo{network: true, name: "a network share"}},
	} {
		if got := windowsFS(c.path, c.drive); got != c.want {
			t.Errorf("windowsFS(%s, %d) = %+v, want %+v", c.path, c.drive, got, c.want)
		}
	}
	for path, want := range map[string]string{
		"/home/me/.config/schwabtx/history.db":                                 "",
		"/home/me/Dropbox/history.db":                                          "Dropbox",
		"/home/me/Dropbox (Personal)/schwabtx/history.db":                      "Dropbox",
		"/home/me/Dropboxes/history.db":                                        "",
		`C:\Users\me\OneDrive - Contoso\history.db`:                            "OneDrive",
		"/Users/me/Google Drive/history.db":                                    "Google Drive",
		"/Users/me/iCloudDrive/history.db":                                     "iCloudDrive",
		"/Users/me/Library/Mobile Documents/com~apple~CloudDocs/history.db":    "iCloud Drive",
		"/Users/me/Library/CloudStorage/GoogleDrive-me@example.com/history.db": "a cloud storage folder",
		"/Users/me/Library/Application Support/schwabtx/history.db":            "",
	} {
		if got := syncedFolder(path); got != want {
			t.Errorf("syncedFolder(%s) = %q, want %q", path, got, want)
		}
	}
}

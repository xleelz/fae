//go:build linux

package commandshim

import "syscall"

// ramFileSystem reports whether path is on a RAM file system (tmpfs or ramfs).
func ramFileSystem(path string) bool {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return true // unknown: treated as RAM-backed
	}
	return uint32(st.Type) == 0x01021994 || uint32(st.Type) == 0x858458f6
}

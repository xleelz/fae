//go:build !linux

package commandshim

// ramFileSystem reports whether path is on a RAM file system; off Linux the
// shared temporary directory is taken to be on disk.
func ramFileSystem(string) bool { return false }

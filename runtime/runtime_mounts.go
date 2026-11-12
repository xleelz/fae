package runtime

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RuntimeMountIdentities hashes a deterministic listing, including symlink
// destinations and file sizes/modes/timestamps. Runtime contents remain pinned
// read-only in the view and are never copied into task output trees.
func RuntimeMountIdentities(paths, forbidden []string) (map[string]string, error) {
	result := map[string]string{}
	overlap := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return nil, fmt.Errorf("runtime mount must be an absolute non-root directory: %s", path)
		}
		physical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(physical)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("runtime mount is not a directory: %s", path)
		}
		for _, other := range forbidden {
			if other == "" {
				continue
			}
			other = filepath.Clean(other)
			if resolved, e := filepath.EvalSymlinks(other); e == nil {
				other = resolved
			}
			if overlap(physical, other) || overlap(filepath.Clean(path), other) {
				return nil, fmt.Errorf("runtime mount %s overlaps protected path %s", path, other)
			}
		}
		hash := sha256.New()
		err = filepath.WalkDir(physical, func(full string, entry fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			relative, e := filepath.Rel(physical, full)
			if e != nil {
				return e
			}
			link := ""
			if info.Mode()&os.ModeSymlink != 0 {
				link, e = os.Readlink(full)
				if e != nil {
					return e
				}
			}
			fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\x00%s\n", filepath.ToSlash(relative), info.Mode(), info.Size(), info.ModTime().UnixNano(), link)
			return nil
		})
		if err != nil {
			return nil, err
		}
		result[path] = fmt.Sprintf("%x", hash.Sum(nil))
	}
	return result, nil
}

// PrivateCacheDirectories allocates stable caches beneath a task-owned state
// root. Its returned paths, not the configured shared directories, are writable.
func PrivateCacheDirectories(stateRoot string, paths []string) (map[string]string, error) {
	result := map[string]string{}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return nil, fmt.Errorf("cache directory must be absolute and non-root")
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
		target := filepath.Join(stateRoot, "cache", key)
		if err := os.MkdirAll(target, 0700); err != nil {
			return nil, err
		}
		physical, err := filepath.EvalSymlinks(target)
		if err != nil || physical != target {
			return nil, fmt.Errorf("private cache directory is not physical")
		}
		result[path] = target
	}
	return result, nil
}
func ApplyPrivateCaches(env []string, caches map[string]string) []string {
	out := append([]string{}, env...)
	for i, entry := range out {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "XDG_CACHE_HOME":
			for configured, private := range caches {
				if value == configured || strings.HasPrefix(value, configured+string(filepath.Separator)) {
					out[i] = key + "=" + private + strings.TrimPrefix(value, configured)
					break
				}
			}
		}
	}
	return out
}

func ValidateCacheDirectories(paths, forbidden []string) error {
	clean := func(path string) string {
		path = filepath.Clean(path)
		if actual, err := filepath.EvalSymlinks(path); err == nil {
			return actual
		}
		return path
	}
	overlap := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
	}
	for i, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return fmt.Errorf("cache directory must be absolute and non-root")
		}
		for _, other := range append(append([]string{}, forbidden...), paths[:i]...) {
			if other != "" && overlap(clean(path), clean(other)) {
				return fmt.Errorf("cache directory %s overlaps protected path %s", path, other)
			}
		}
	}
	return nil
}

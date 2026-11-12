package provider

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
func viewFileDigest(file directoryFile) string {
	if file.digest != "" {
		return file.digest
	}
	return fmt.Sprintf("%x", sha256.Sum256(file.data))
}
func snapshotViewDirectory(root string) (DirectorySnapshot, error) {
	snapshot := DirectorySnapshot{}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := directoryFile{mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(full)
			if e != nil {
				return e
			}
			file.data = []byte(target)
		} else if info.Mode().IsRegular() {
			file.digest, err = fileDigest(full)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported view file %s", relative)
		}
		snapshot[relative] = file
		return nil
	})
	return snapshot, err
}
func copyCollectedFile(source, target string, mode os.FileMode, expected string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temp, err := os.CreateTemp(filepath.Dir(target), ".fae-collect-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(temp, hash), input)
	closeErr := temp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return fmt.Errorf("private file changed during collection")
	}
	if err = os.Chmod(temp.Name(), mode.Perm()); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}
func digestViewChanges(root string, before, after DirectorySnapshot, names []string) ([]string, string, error) {
	hash := sha256.New()
	changed := []string{}
	for _, name := range names {
		a, aok := before[name]
		b, bok := after[name]
		if aok == bok && a.mode == b.mode && viewFileDigest(a) == viewFileDigest(b) {
			continue
		}
		relative := filepath.ToSlash(name)
		changed = append(changed, relative)
		if !bok {
			fmt.Fprintf(hash, "%s\\x00deleted\\x00", relative)
			continue
		}
		fmt.Fprintf(hash, "%s\\x00%d\\x00", relative, b.mode)
		if b.mode&os.ModeSymlink != 0 {
			_, _ = hash.Write(b.data)
		} else {
			input, err := os.Open(filepath.Join(root, name))
			if err != nil {
				return nil, "", err
			}
			_, err = io.Copy(hash, input)
			input.Close()
			if err != nil {
				return nil, "", err
			}
		}
	}
	return changed, fmt.Sprintf("%x", hash.Sum(nil)), nil
}
func physicalWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

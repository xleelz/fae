package effects

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// This identifies the executable observed before launch, not its entire
// dependency/toolchain closure. Runtime mount pins cover configured tool roots.
func formalExecutableDigest(binary string) (string, error) {
	path, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<20 {
		return "", fmt.Errorf("formal executable must be a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	if err != nil || n > 256<<20 {
		return "", fmt.Errorf("formal executable identity could not be read: %v", err)
	}
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil)), nil
}

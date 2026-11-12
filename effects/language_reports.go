package effects

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

func readPythonReport(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > DefaultMaxOutput {
		return nil, fmt.Errorf("pytest report must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, DefaultMaxOutput+1))
	if len(data) > DefaultMaxOutput {
		return nil, fmt.Errorf("pytest report exceeds evidence bound")
	}
	return data, err
}

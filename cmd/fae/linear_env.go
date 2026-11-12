package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load host-local configuration without executing shell code or overriding the
// environment supplied by the service manager. Missing files are optional.
func loadLinearEnv() error {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil // Offline Symphony can run without a home directory.
	}
	return loadLinearEnvFile(filepath.Join(dir, "fae", "linear.env"))
}

func loadLinearEnvFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Linear environment file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat Linear environment file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("Linear environment file must be a private regular file (chmod 600 %s)", path)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		s := strings.TrimSpace(scanner.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, value, ok := strings.Cut(s, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		valid := key != ""
		for i, c := range key {
			if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
				valid = false
			}
		}
		if !ok || !valid || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid Linear environment assignment on line %d", line)
		}
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return fmt.Errorf("invalid Linear environment quoting on line %d", line)
			}
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return errors.New("cannot read Linear environment file")
	}
	for key, value := range values {
		if _, present := os.LookupEnv(key); !present {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("cannot set Linear environment variable %s", key)
			}
		}
	}
	return nil
}

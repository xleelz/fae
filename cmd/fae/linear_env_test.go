package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinearEnvStartupAndPrecedence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fae", "linear.env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const key = "FAE_TEST_LINEAR_FROM_FILE"
	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })
	t.Setenv("FAE_TEST_LINEAR_EXISTING", "service")
	if err := os.WriteFile(path, []byte("# comment\nexport "+key+"='literal$(no-command)$VAR'\nFAE_TEST_LINEAR_EXISTING=file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadLinearEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(key) != "literal$(no-command)$VAR" || os.Getenv("FAE_TEST_LINEAR_EXISTING") != "service" {
		t.Fatal("file values or service precedence incorrect")
	}
}

func TestLinearEnvInvalidFileDoesNotLeakOrPartiallyApply(t *testing.T) {
	const key = "FAE_TEST_LINEAR_INVALID"
	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })
	path := filepath.Join(t.TempDir(), "linear.env")
	if err := loadLinearEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{key + "=secret\nmalformed-secret", key + "='secret", "BAD-KEY=secret"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		err := loadLinearEnvFile(path)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("expected redacted validation failure")
		}
		if _, ok := os.LookupEnv(key); ok {
			t.Fatal("invalid file partially applied")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := loadLinearEnvFile(path); err == nil {
		t.Fatal("accepted publicly readable secret file")
	}
}

package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, p := range []string{"/", "/tmp/a", "/hello world"} {
		if err := Validate(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"", "a", "/a/", "/a//b", "/a/./b", "/a/../b", "/a\x00b", string([]byte{'/', 255})} {
		if Validate(p) == nil {
			t.Errorf("accepted %q", p)
		}
	}
}
func TestContainment(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	file := filepath.Join(root, "a")
	if err := os.WriteFile(file, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Contained(root, file); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if Contained(root, link) == nil {
		t.Fatal("symlink escape accepted")
	}
	if Contained(root, filepath.Join(root, "missing")) == nil {
		t.Fatal("missing accepted")
	}
}
func TestBinding(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckBinding(root, root, root); err != nil {
		t.Fatal(err)
	}
	if CheckBinding(root, root, "/different") == nil {
		t.Fatal("rebound")
	}
	if CheckBinding(root, "/different", "") == nil {
		t.Fatal("wrong cwd")
	}
}

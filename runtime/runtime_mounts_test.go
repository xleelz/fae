package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeMountIdentitiesRefuseOverlapAndDetectListingChange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tool"), []byte("a"), 0700); err != nil {
		t.Fatal(err)
	}
	first, err := RuntimeMountIdentities([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RuntimeMountIdentities([]string{root}, []string{filepath.Join(root, "task")}); err == nil {
		t.Fatal("overlapping task accepted")
	}
	if err = os.WriteFile(filepath.Join(root, "tool2"), []byte("b"), 0700); err != nil {
		t.Fatal(err)
	}
	second, err := RuntimeMountIdentities([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first[root] == second[root] {
		t.Fatal("listing change did not alter identity")
	}
}
func TestPrivateCachesStableWithinTaskAndSeparateAcrossTasks(t *testing.T) {
	configured := []string{"/configured/cache"}
	root := t.TempDir()
	first, err := PrivateCacheDirectories(filepath.Join(root, "task1"), configured)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PrivateCacheDirectories(filepath.Join(root, "task1"), configured)
	if err != nil {
		t.Fatal(err)
	}
	other, err := PrivateCacheDirectories(filepath.Join(root, "task2"), configured)
	if err != nil {
		t.Fatal(err)
	}
	if first[configured[0]] != again[configured[0]] || first[configured[0]] == other[configured[0]] {
		t.Fatal("cache lifetime or task isolation incorrect")
	}
	env := ApplyPrivateCaches([]string{"GOCACHE=/configured/cache/go"}, first)
	if env[0] != "GOCACHE="+first[configured[0]]+"/go" {
		t.Fatal(env)
	}
}

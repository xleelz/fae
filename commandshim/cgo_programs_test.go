package commandshim

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The refusal of a program a nested go would find by name on the call's PATH
// where the run's PATH has none (shim.go), in the census corpus
// (refusal_corpus_test.go).
func init() {
	shimRefusals[`"nested go would run " + found + " for " + key + ", a program the call's PATH chose: the run's PATH has no " + name + " to run instead; nested calls run only the run's own programs go finds by name"`] = struct {
		reason string
		run    func(t *testing.T, p Policy) error
	}{"for PKG_CONFIG, a program the call's PATH chose: the run's PATH has no pkg-config", func(t *testing.T, p Policy) error {
		bin := t.TempDir()
		write(t, filepath.Join(bin, "pkg-config"), "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(bin, "pkg-config"), 0700); err != nil {
			t.Fatal(err)
		}
		_, e := p.accountedGoEnv([]string{"go", "build", "-p=2", "."}, []string{"PATH=" + bin}, &NestedGo{Programs: map[string]string{}})
		return e
	}}
}

// cgoBin makes a directory holding an executable for each name.
func cgoBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		write(t, filepath.Join(dir, name), "#!/bin/sh\nexit 1\n")
		if err := os.Chmod(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// FAE-HOST-014: a nested go call finds the C compiler, the C++ and Fortran
// compilers and pkg-config by name on its PATH, which is the test's. Each
// is pinned to the run's own (NestedGoPrograms, from the run's PATH): CC,
// CXX, FC and PKG_CONFIG name its absolute path, after the run's own value's
// arguments are kept, and a call it starts again may keep them. A program
// neither PATH has stays unset (so go turns cgo off without a compiler, as
// before); one only the call's PATH has is refused.
func TestAccountedGoPinsTheProgramsGoFindsByName(t *testing.T) {
	p, _ := accountedGoRun(t)
	run := cgoBin(t, "gcc", "g++", "clang", "pkg-config")
	call := cgoBin(t, "gcc", "g++", "clang", "pkg-config")
	g := &NestedGo{Programs: NestedGoPrograms([]string{"PATH=" + run})}
	at := func(name string) string { return filepath.Join(run, name) }
	if want := map[string]string{"gcc": at("gcc"), "g++": at("g++"), "clang": at("clang"), "pkg-config": at("pkg-config")}; !reflect.DeepEqual(g.Programs, want) {
		t.Fatalf("NestedGoPrograms = %v, want %v", g.Programs, want)
	}
	build := []string{"go", "build", "-p=2", "."}
	pins := func(t *testing.T, g *NestedGo, env []string, want map[string]string) {
		t.Helper()
		got, err := p.accountedGoEnv(build, env, g)
		if err != nil {
			t.Fatalf("%q: %v", env, err)
		}
		for _, key := range cgoProgramKeys {
			v, ok := lastGoEnv(got, key)
			if w, pinned := want[key]; ok != pinned || v != w || ok && slices.IndexFunc(got, func(kv string) bool { return strings.HasPrefix(kv, key+"=") }) != slices.IndexFunc(got, func(kv string) bool { return kv == key+"="+v }) {
				t.Errorf("%q: %s=%q (set %t), want %q (set %t) once", env, key, v, ok, w, pinned)
			}
		}
	}
	// A test's PATH names its own gcc, g++ and pkg-config: the run's run.
	pins(t, g, []string{"PATH=" + call}, map[string]string{"CC": at("gcc"), "CXX": at("g++"), "PKG_CONFIG": at("pkg-config")})
	pins(t, g, []string{"PATH=" + call, "CC=", "PKG_CONFIG="}, map[string]string{"CC": at("gcc"), "CXX": at("g++"), "PKG_CONFIG": at("pkg-config")})
	// A darwin target's default C compiler is clang.
	pins(t, g, []string{"PATH=" + call, "GOOS=darwin"}, map[string]string{"CC": at("clang"), "PKG_CONFIG": at("pkg-config")})
	// Without a compiler on the call's PATH, nothing is set: go turns cgo off.
	pins(t, g, []string{"PATH=" + t.TempDir()}, nil)
	pins(t, g, nil, nil)
	// The run's own CC with arguments keeps them, pinned; a call it starts
	// inherits that value and keeps it.
	own := &NestedGo{Env: map[string]string{"CC": "gcc -m64"}, Programs: g.Programs}
	pins(t, own, []string{"PATH=" + call, "CC=gcc -m64"}, map[string]string{"CC": at("gcc") + " -m64", "CXX": at("g++"), "PKG_CONFIG": at("pkg-config")})
	pins(t, own, []string{"PATH=" + call, "CC=" + at("gcc") + " -m64"}, map[string]string{"CC": at("gcc") + " -m64", "CXX": at("g++"), "PKG_CONFIG": at("pkg-config")})
	pins(t, g, []string{"PATH=" + call, "CC=" + at("gcc"), "CXX=" + at("g++")}, map[string]string{"CC": at("gcc"), "CXX": at("g++"), "PKG_CONFIG": at("pkg-config")})
	// A pinned path with other arguments, or another program, is the call's
	// own choice and refused as before; so is a program only the call's PATH has.
	for _, c := range []struct {
		g      *NestedGo
		env    []string
		reason string
	}{
		{own, []string{"PATH=" + call, "CC=" + at("gcc") + " -m32"}, "nested go with CC=" + at("gcc") + " -m32: CC may name a program go runs"},
		{g, []string{"PATH=" + call, "CC=" + filepath.Join(call, "gcc")}, "nested go with CC=" + filepath.Join(call, "gcc") + ": CC may name a program go runs"},
		{g, []string{"PATH=" + cgoBin(t, "gfortran")}, "for FC, a program the call's PATH chose: the run's PATH has no gfortran"},
		{&NestedGo{Programs: map[string]string{}}, []string{"PATH=" + call}, "for CC, a program the call's PATH chose: the run's PATH has no gcc"},
	} {
		_, err := p.accountedGoEnv(build, c.env, c.g)
		var refused *Refusal
		if !errors.As(err, &refused) || !strings.Contains(refused.Reason, c.reason) {
			t.Errorf("%q: want refusal %q, got %v", c.env, c.reason, err)
		}
	}
	// go version runs no C toolchain, so nothing is pinned or refused for it.
	if got, err := p.accountedGoEnv([]string{"go", "version"}, []string{"PATH=" + call}, &NestedGo{Programs: map[string]string{}}); err != nil || slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "CC=") }) {
		t.Errorf("go version: %q %v", got, err)
	}
}

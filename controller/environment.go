package controller

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A launch environment carries provider, plane and instance credentials
// (FAE-SEC-003). Markers, plane rows and events name it only by digest; the
// values live in an owner-only file in the instance root until the relaunch
// they authorize is accepted.

func environmentPath(dir, digest string) (string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", errors.New("invalid launch environment digest")
	}
	return filepath.Join(dir, "controller-environment-"+digest+".json"), nil
}

// SealEnvironment moves inline environment values into a private file named by
// their digest, leaving only the digest in the spec.
func SealEnvironment(dir string, spec *LaunchSpec) error {
	if len(spec.Environment) == 0 {
		return nil
	}
	digest := EnvironmentHash(spec.Environment)
	if spec.EnvironmentDigest != "" && spec.EnvironmentDigest != digest {
		return errors.New("launch environment differs from its digest")
	}
	path, _ := environmentPath(dir, digest)
	if _, e := readEnvironment(path, digest); e != nil {
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		raw, _ := json.Marshal(spec.Environment)
		f, e := os.CreateTemp(dir, ".controller-environment-")
		if e != nil {
			return e
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		if closeErr := f.Close(); e == nil {
			e = closeErr
		}
		if e == nil {
			e = os.Rename(f.Name(), path)
		}
		if e != nil {
			_ = os.Remove(f.Name())
			return e
		}
	}
	spec.Environment, spec.EnvironmentDigest = nil, digest
	return nil
}

// ResolveEnvironment returns the spec with its sealed environment restored and
// verified. A spec recorded before sealing carries its values inline.
func ResolveEnvironment(dir string, spec LaunchSpec) (LaunchSpec, error) {
	if spec.EnvironmentDigest == "" {
		if len(spec.Environment) == 0 {
			return spec, errors.New("launch environment missing")
		}
		return spec, nil
	}
	path, e := environmentPath(dir, spec.EnvironmentDigest)
	if e != nil {
		return spec, e
	}
	env, e := readEnvironment(path, spec.EnvironmentDigest)
	if e != nil {
		return spec, e
	}
	if len(spec.Environment) > 0 && EnvironmentHash(spec.Environment) != spec.EnvironmentDigest {
		return spec, errors.New("launch environment differs from its digest")
	}
	spec.Environment = env
	return spec, nil
}

// Digest is the identity a relaunched daemon's environment must match.
func (s LaunchSpec) Digest() string {
	if s.EnvironmentDigest != "" {
		return s.EnvironmentDigest
	}
	return EnvironmentHash(s.Environment)
}

// DiscardEnvironments removes sealed environments in dir, except those named in keep.
func DiscardEnvironments(dir string, keep ...string) error {
	names, e := filepath.Glob(filepath.Join(dir, "controller-environment-*.json"))
	if e != nil {
		return e
	}
	// Interrupted seals leave only unreferenced temporary files.
	partial, _ := filepath.Glob(filepath.Join(dir, ".controller-environment-*"))
	names = append(names, partial...)
	var errs []error
	for _, name := range names {
		retained := false
		for _, digest := range keep {
			retained = retained || name == filepath.Join(dir, "controller-environment-"+digest+".json")
		}
		if !retained {
			if e := os.Remove(name); e != nil && !os.IsNotExist(e) {
				errs = append(errs, e)
			}
		}
	}
	return errors.Join(errs...)
}

func readEnvironment(path, digest string) ([]string, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return nil, errors.New("invalid private launch environment")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil {
		return nil, e
	}
	var env []string
	if e = json.Unmarshal(raw, &env); e != nil || len(env) == 0 || EnvironmentHash(env) != digest {
		return nil, errors.New("private launch environment does not match its digest")
	}
	return env, nil
}

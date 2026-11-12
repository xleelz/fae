package scheduler

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// OperationProof links both durable pools to a launch boundary. The journal is
// protected by the daemon's existing singleton lock, never by elapsed time.
type OperationProof struct {
	ID, Session, Instance, Birth, RolePool, State string
	ProviderPool                                  string `json:",omitempty"`
	Registry, ProcessOwner                        string
	Owner                                         Owner
	Expected                                      int
}
type Journal struct {
	root string
	mu   sync.Mutex
}

func OpenJournal(root string) (*Journal, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("absolute journal required")
	}
	for p := root; ; p = filepath.Dir(p) {
		s, e := os.Lstat(p)
		if e == nil && s.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("journal symlink")
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	s, e := os.Stat(root)
	if e != nil {
		return nil, e
	}
	if s.Mode().Perm() != 0700 {
		return nil, errors.New("private journal required")
	}
	return &Journal{root: root}, nil
}
func validProof(p OperationProof) bool {
	return p.ID != "" && !strings.ContainsAny(p.ID, "/\\.") && p.Session != "" && p.Instance != "" && p.Birth != "" && p.Owner.PID > 1 && p.Owner.Started > 0 && (p.State == "reserved" || p.State == "launching" || p.State == "retired")
}
func (j *Journal) Put(p OperationProof) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !validProof(p) {
		return errors.New("invalid operation proof")
	}
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(j.root, "pending-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, filepath.Join(j.root, p.ID+".json")); e != nil {
		return e
	}
	d, e := os.Open(j.root)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (j *Journal) Read(id string) (OperationProof, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var p OperationProof
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return p, errors.New("invalid proof id")
	}
	f, e := os.OpenFile(filepath.Join(j.root, id+".json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return p, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return p, e
	}
	if !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > 65536 {
		return p, errors.New("invalid journal record")
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if e = dec.Decode(&p); e != nil {
		return p, e
	}
	var extra any
	if e = dec.Decode(&extra); e != io.EOF {
		return p, errors.New("trailing journal data")
	}
	if !validProof(p) || p.ID != id {
		return p, errors.New("invalid operation proof")
	}
	return p, nil
}
func (j *Journal) Entries() ([]OperationProof, error) {
	paths, e := filepath.Glob(filepath.Join(j.root, "*.json"))
	if e != nil {
		return nil, e
	}
	var out []OperationProof
	for _, path := range paths {
		p, e := j.Read(strings.TrimSuffix(filepath.Base(path), ".json"))
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
func (j *Journal) Remove(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return errors.New("invalid proof id")
	}
	e := os.Remove(filepath.Join(j.root, id+".json"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	d, e := os.Open(j.root)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

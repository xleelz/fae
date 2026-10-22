package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/xleelz/fae/processowner"
)

type residentOwner struct {
	Owner, Session, ParentBirth, Socket, Request string
	ParentPID                                    int
}

func writeResidentOwner(root string, r *residentEntry) error {
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		return e
	}
	raw, e := json.Marshal(residentOwner{Owner: root, Session: r.id, ParentPID: os.Getpid(), ParentBirth: birth, Socket: r.socket, Request: r.request})
	if e != nil {
		return e
	}
	file, e := os.OpenFile(filepath.Join(r.dir, "resident-owner.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(raw)
	if e == nil {
		e = file.Sync()
	}
	e = errors.Join(e, file.Close())
	if e != nil {
		return e
	}
	dir, e := os.Open(r.dir)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}

// RecoverResidentSockets removes only sockets backed by a private durable marker
// and a complete pair of terminal resident process receipts. Call after Recover
// and before acknowledging its receipts. A pathname alone never proves ownership.
func RecoverResidentSockets(root string, recovered []processowner.Record) error {
	type identity struct {
		owner, session, birth string
		pid                   int
	}
	byIdentity := map[identity][]processowner.Record{}
	for _, record := range recovered {
		if strings.HasPrefix(record.Session, "resident-") {
			key := identity{record.Owner, record.Session, record.ParentBirth, record.ParentPID}
			byIdentity[key] = append(byIdentity[key], record)
		}
	}
	if len(byIdentity) == 0 {
		return nil
	}
	if e := canonicalDirectory(root); e != nil {
		return e
	}
	paths, e := filepath.Glob(filepath.Join(root, "resident-*", "resident-owner.json"))
	if e != nil {
		return e
	}
	for _, path := range paths {
		parent := filepath.Dir(path)
		if e = canonicalDirectory(parent); e != nil {
			return e
		}
		dirInfo, e := os.Stat(parent)
		if e != nil {
			return e
		}
		if dirInfo.Mode().Perm() != 0700 {
			return errors.New("resident recovery requires private directory")
		}
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
			return errors.New("invalid resident ownership marker")
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var marker residentOwner
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&marker); e != nil {
			return e
		}
		records := byIdentity[identity{marker.Owner, marker.Session, marker.ParentBirth, marker.ParentPID}]
		if len(records) == 0 {
			continue
		}
		if marker.Owner != root || marker.ParentPID <= 1 || marker.ParentBirth == "" || marker.Socket != filepath.Join(parent, "mux.sock") || marker.Request != filepath.Join(parent, "request.sock") {
			return errors.New("resident recovery marker ownership mismatch")
		}
		if len(records) != 2 {
			return errors.New("resident recovery requires complete server and pane retirement")
		}
		for _, record := range records {
			if record.Owner != root || record.ParentPID != marker.ParentPID || record.ParentBirth != marker.ParentBirth {
				return errors.New("resident recovery process ownership mismatch")
			}
		}
		proved, e := processowner.LinkedRetirement(filepath.Join(root, ".processes"), root, marker.Session, marker.ParentPID, marker.ParentBirth, 2)
		if e != nil {
			return e
		}
		if !proved {
			return errors.New("resident recovery retirement unproven")
		}
		for _, socket := range []string{marker.Socket, marker.Request} {
			info, e := os.Lstat(socket)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSocket == 0 {
				return errors.New("resident recovery target is not a socket")
			}
			if e = os.Remove(socket); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
		dir, e := os.Open(parent)
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
	}
	return nil
}

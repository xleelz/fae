package daemon

import (
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// PrincipalCredential binds a distinct owner-only credential file to its authority.
type PrincipalCredential struct {
	CredentialFile string `json:"credential_file"`
	Actor          string `json:"actor"`
	Task           string `json:"task,omitempty"`
}

func installPrincipals(server *api.Server, path string) error {
	if path == "" {
		return nil
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 {
		return errors.New("principal configuration must be a bounded owner-only regular file")
	}
	var entries []PrincipalCredential
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&entries); e != nil {
		return e
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing principal configuration")
	}
	for _, entry := range entries {
		if !filepath.IsAbs(entry.CredentialFile) {
			return errors.New("delegated credential path must be absolute")
		}
		credential, e := api.Credential(entry.CredentialFile)
		if e != nil {
			return e
		}
		if e = server.AddPrincipal(credential, api.Principal{Actor: entry.Actor, Task: entry.Task}); e != nil {
			return e
		}
	}
	return nil
}

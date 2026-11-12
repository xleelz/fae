package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/api"
	"os"
	"path/filepath"
	"testing"
)

func TestDelegatedConfigurationSafety(t *testing.T) {
	for _, tc := range []struct {
		name, actor, path string
		mode              os.FileMode
		valid             bool
	}{
		{"valid", "controller", "absolute", 0600, true}, {"public config", "controller", "absolute", 0644, false}, {"operator spoof", "operator", "absolute", 0600, false}, {"relative credential", "controller", "relative", 0600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := outsideCheckout(t)
			path := tc.path
			if path == "absolute" {
				path = filepath.Join(root, "credential")
			}
			raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: path, Actor: tc.actor}})
			cfg := filepath.Join(root, "config.json")
			if e := os.WriteFile(cfg, raw, tc.mode); e != nil {
				t.Fatal(e)
			}
			s, _ := api.New(make([]byte, 32), func(context.Context, api.Request) (any, error) { return nil, nil })
			e := installPrincipals(s, cfg)
			if (e == nil) != tc.valid {
				t.Fatal(e)
			}
			if tc.valid {
				info, e := os.Stat(path)
				if e != nil || info.Mode().Perm() != 0600 || info.Size() != 32 {
					t.Fatalf("unsafe generated credential %v", e)
				}
			}
		})
	}
}

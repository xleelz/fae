package controlplane

import (
	"context"
	"strings"
	"testing"
)

// FAE-CP-008: writes outside Mutate are Dolt commits too: each migration names
// its version and a provider observation names itself and its actor.
func TestMigrationsAndProviderObservationsAreCommits(t *testing.T) {
	s := testPlane(t, 7)
	ctx := context.Background()
	s.options.VersionCommits = true
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.ObserveProvider(ctx, observation()); e != nil {
		t.Fatal(e)
	}
	if e := s.ObserveProvider(ctx, observation()); e != nil { // an identical replay writes nothing
		t.Fatal(e)
	}
	commits, e := s.Commits(ctx, 10)
	if e != nil || len(commits) < 3 {
		t.Fatal(commits, e)
	}
	if !strings.HasPrefix(commits[0].Message, "provider observation ") || !strings.HasSuffix(commits[0].Message, " by daemon") {
		t.Fatalf("observation commit %q", commits[0].Message)
	}
	if commits[1].Message != "migration 11 by daemon" || commits[2].Message != "migration 10 by daemon" {
		t.Fatalf("migration commits %q %q", commits[1].Message, commits[2].Message)
	}
	keys, e := s.ChangedKeys(ctx, commits[0].Hash, "provider_health", "provider")
	if e != nil || len(keys) != 1 || keys[0] != observation().Provider {
		t.Fatal(keys, e)
	}
}

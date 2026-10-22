package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"path/filepath"
)

func (a *liveAdmission) proofID(session string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(a.daemon.layout.Root+"\x00"+session)))
}
func (a *liveAdmission) Launching(ctx context.Context, session string) error {
	if a.journal == nil {
		return nil
	}
	p, e := a.journal.Read(a.proofID(session))
	if e != nil {
		return e
	}
	if p.Owner != a.owner || p.Birth != a.birth || p.State != "reserved" {
		return errors.New("invalid durable launch boundary")
	}
	p.State = "launching"
	return a.journal.Put(p)
}
func (a *liveAdmission) Retired(ctx context.Context, session string, proven bool) error {
	if a.journal == nil || !proven {
		return nil
	}
	p, e := a.journal.Read(a.proofID(session))
	if e != nil {
		return e
	}
	if p.Owner != a.owner || p.Birth != a.birth || p.State != "launching" {
		return errors.New("invalid retirement boundary")
	}
	p.State = "retired"
	if e = a.journal.Put(p); e != nil {
		return e
	}
	if p.Expected > 0 {
		return processowner.AcknowledgeLinkedRetirement(p.Registry, p.ProcessOwner, p.Session, p.Owner.PID, p.Birth)
	}
	return nil
}
func (a *liveAdmission) checkedRelease(id string, release func() error) func() error {
	return func() error {
		if a.journal != nil {
			p, e := a.journal.Read(id)
			if e != nil {
				return e
			}
			if p.State == "launching" {
				return errors.New("lease retained: workload retirement unproven")
			}
		}
		if e := release(); e != nil {
			return e
		}
		if a.journal != nil {
			return a.journal.Remove(id)
		}
		return nil
	}
}
func (a *liveAdmission) recoverProofs(ctx context.Context) error {
	entries, e := a.journal.Entries()
	if e != nil {
		return e
	}
	for _, p := range entries {
		if p.Instance != a.daemon.layout.Root {
			return errors.New("foreign lease proof instance")
		}
		dead := processowner.ProvedDead(p.Owner.PID, p.Birth)
		if dead && p.State == "launching" && p.Expected > 0 {
			sessions := filepath.Join(a.daemon.layout.Root, "sessions")
			validRegistry := (p.Registry == filepath.Join(sessions, ".processes") && p.ProcessOwner == sessions) || (p.Registry == filepath.Join(a.daemon.layout.Run, "effect-processes") && p.ProcessOwner == a.daemon.layout.Worktrees)
			if !validRegistry {
				return errors.New("foreign process registry in lease proof")
			}
			complete, e := processowner.LinkedRetirement(p.Registry, p.ProcessOwner, p.Session, p.Owner.PID, p.Birth, p.Expected)
			if e != nil {
				return e
			}
			if complete {
				p.State = "retired"
				if e = a.journal.Put(p); e != nil {
					return e
				}
			}
		}
		if !scheduler.CanRecoverLease(dead, true, p.State == "reserved", p.State == "retired") {
			continue
		}
		audit := controlplane.Audit{Actor: "daemon", Action: "durable lease recovery", Outcome: "retired"}
		// Role first, host second; retaining the journal makes partial recovery replayable.
		if p.RolePool != "" {
			if e = a.daemon.store.RecoverLease(ctx, audit, p.RolePool, p, dead); e != nil {
				return e
			}
		}
		if e = a.host.Recover(ctx, p, dead); e != nil {
			return e
		}
		if p.State == "retired" && p.Expected > 0 {
			if e = processowner.AcknowledgeLinkedRetirement(p.Registry, p.ProcessOwner, p.Session, p.Owner.PID, p.Birth); e != nil {
				return e
			}
		}
		if e = a.journal.Remove(p.ID); e != nil {
			return e
		}
	}
	return nil
}

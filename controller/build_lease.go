package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// BuildLease is the host admission a controller package build runs under.
// FAE-SCH-011 clause 7: controller heavy ops take the same lease as daemon
// work and are never exempt; FAE-HOST-001, -007, -010: the build samples the
// host, waits (bounded) for the landing floor and the two-reading I/O gate,
// and reconciles dead ledger owners (FAE-HOST-031). Ledger is the instance's
// host ledger directory; the other fields are test seams over the host.
type BuildLease struct {
	Ledger  string
	Log     io.Writer
	Observe func(context.Context) (hostguard.Sample, error)
	Clock   hostguard.Clock
	Policy  hostguard.Policy
	Probe   func(scheduler.Owner) scheduler.Liveness
	MaxWait time.Duration
}

// Appendix A weight of a package build and its build jobs (FAE-HOST-011).
const (
	packageBuildWeight = 2
	packageBuildJobs   = 2
)

// hold runs fn holding the lease after host admission. It waits FIFO behind
// earlier requests, bounded, and never runs fn unadmitted.
func (b BuildLease) hold(ctx context.Context, operation string, fn func() error) error {
	if !filepath.IsAbs(b.Ledger) {
		return errors.New("controller build requires the instance's host heavy-op ledger")
	}
	ledger, e := scheduler.OpenDynamicLedger(b.Ledger, packageBuildWeight)
	if e != nil {
		return e
	}
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		return e
	}
	pool, e := ledger.Snapshot(ctx)
	if e != nil {
		return e
	}
	_, cfg, e := ledger.HostLimits(ctx)
	if e != nil {
		return e
	}
	// A pool smaller than the declared weight is taken whole, never bypassed.
	weight := min(packageBuildWeight, pool.Capacity)
	if pool.AdmissionLimit > 0 {
		weight = min(weight, pool.AdmissionLimit)
	}
	nonce := make([]byte, 8)
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	log := b.Log
	if log == nil {
		log = os.Stderr
	}
	observe := b.Observe
	if observe == nil {
		observe = hostguard.Observe
	}
	admission := HeavyAdmission{Ledger: ledger, Owner: owner, Observe: observe, Clock: b.Clock, Policy: b.Policy, Config: cfg, MemoryFloor: LandingFloor, Weight: weight, Class: "controller", MaxWait: b.MaxWait, Probe: b.Probe,
		Alert: func(line string) { fmt.Fprintf(log, "fae: controller %s %s\n", operation, line) }}
	release, e := admission.Acquire(ctx, "controller-"+operation+"-"+hex.EncodeToString(nonce))
	if e != nil {
		return fmt.Errorf("controller %s admission: %w", operation, e)
	}
	return errors.Join(fn(), release())
}

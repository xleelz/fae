package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"time"
)

func (d *Daemon) deploymentHolds(ctx context.Context, p controller.Params, h *controller.Health, a controlplane.Audit) error {
	if d.backlogUncertain {
		return errors.New("backlog commit must reconcile before deployment")
	}
	bound := p.OverlapWait
	if bound == 0 {
		bound = 2 * time.Hour
	}
	if bound < 0 {
		return errors.New("overlap wait must be positive")
	}
	top := 0
	for _, snapshot := range d.jobs {
		var env envelope
		if decodeEnvelope(snapshot.Payload, &env) != nil {
			continue
		}
		if _, ok := env.Job.Document.Stages[string(env.Job.State)]; ok && env.Admission.Priority > top {
			top = env.Admission.Priority
		}
	}
	unknown := map[string]bool{}
	for _, hold := range h.Holds {
		unknown[hold.Task] = hold.QueryFailed
	}
	h.Holds = nil
	blocked := []string{}
	for _, id := range h.Overlap {
		hold := controller.OverlapHold{Task: id, Since: time.Now().UTC(), QueryFailed: unknown[id]}
		var env envelope
		if snapshot, ok := d.jobs[id]; !ok || decodeEnvelope(snapshot.Payload, &env) != nil {
			hold.QueryFailed = true
		} else {
			hold.Critical = env.Admission.Critical
			hold.TopNearLimit = env.Admission.Priority == top && env.Job.Attempts >= env.Job.AttemptLimit()-1
		}
		binaryHash, e := controller.HashFile(p.Binary)
		if e != nil {
			return e
		}
		digest := sha256.Sum256([]byte(binaryHash + "\x00" + p.GatedRef + "\x00" + id))
		key := fmt.Sprintf("deployment-hold/%x", digest)
		raw, e := d.store.GetMarker(ctx, key)
		if e == nil {
			var old controller.OverlapHold
			if json.Unmarshal(raw, &old) != nil || old.Task != id || old.Since.IsZero() {
				return errors.New("invalid durable deployment hold")
			}
			hold.Since = old.Since
		} else if errors.Is(e, sql.ErrNoRows) {
			raw, _ = json.Marshal(hold)
			a.Action = "controller-overlap-held"
			if e = d.store.PutMarker(ctx, a, key, raw); e != nil {
				return e
			}
		} else {
			return e
		}
		h.Holds = append(h.Holds, hold)
		if !controller.MayReleaseOverlap(p.AckOverlap, time.Since(hold.Since) >= bound, hold.Critical, hold.TopNearLimit, hold.QueryFailed) {
			blocked = append(blocked, id)
		}
	}
	h.Overlap = blocked
	return nil
}

// deploymentExecutable refuses replacement of the running executable pathname.
// A live process's accepted identity is its startup digest, not newly written bytes.
func (d *Daemon) deploymentExecutable() (string, string, error) {
	if d.binaryPath == "" || d.binaryHash == "" {
		return "", "", errors.New("startup executable identity unavailable")
	}
	hash, e := controller.HashFile(d.binaryPath)
	if e != nil {
		return "", "", e
	}
	if hash != d.binaryHash {
		return "", "", errors.New("running executable path changed since startup; restore original binary before deployment")
	}
	return d.binaryPath, d.binaryHash, nil
}

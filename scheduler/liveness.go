package scheduler

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Liveness is deliberately three-valued: uncertainty never frees capacity.
type Liveness uint8

const (
	Unknown Liveness = iota
	Alive
	Dead
)

func processOwner(pid int) (Owner, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return Owner{}, err
	}
	stamp, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "), time.Local)
	if err != nil {
		return Owner{}, err
	}
	return Owner{pid, stamp.Unix()}, nil
}
func CurrentOwner() (Owner, error) { return processOwner(os.Getpid()) }
func ProbeOwner(owner Owner) Liveness {
	if owner.PID <= 0 || owner.Started <= 0 {
		return Unknown
	}
	actual, err := processOwner(owner.PID)
	if err != nil {
		// ps exit 1 with empty stdout means no matching process. Other failures,
		// including sandbox denial and parse failures, are ambiguous.
		if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 && len(e.Stderr) == 0 {
			return Dead
		}
		return Unknown
	}
	if actual != owner {
		return Dead
	}
	return Alive
}

// AdvanceObserved only reclaims owners proved dead. Expiration marks an owner
// overdue; it never authorizes reusing capacity while that owner may still run.
func (p *Pool) AdvanceObserved(now time.Time, observed map[Owner]Liveness) ([]Lease, []Event, error) {
	if observed == nil {
		return nil, nil, fmt.Errorf("liveness observations required")
	}
	return p.Advance(now, func(o Owner) bool { return observed[o] != Dead })
}

// Validate checks an untrusted persisted pool without modifying the caller.
func (p Pool) Validate() error {
	p.Holders = append([]Lease(nil), p.Holders...)
	p.Waiting = append([]Request(nil), p.Waiting...)
	_, _, e := p.Advance(time.Now(), func(Owner) bool { return true })
	return e
}

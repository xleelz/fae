package scheduler

import (
	"fmt"
	"github.com/xleelz/fae/processowner"
	"sort"
	"strings"
	"time"
)

type LedgerReader struct {
	Owner  Owner
	Schema int
}

func readerKey(o Owner) string { return fmt.Sprintf("%d:%d", o.PID, o.Started) }
func recordLedgerReader(s *ledgerState) {
	o, err := CurrentOwner()
	if err != nil {
		return
	}
	if s.Readers == nil {
		s.Readers = map[string]LedgerReader{}
	}
	s.Readers[readerKey(o)] = LedgerReader{Owner: o, Schema: 2}
}
func upgradeWorkflowSchema(s *ledgerState) error {
	if s.Version == 2 {
		return nil
	}
	participants := map[Owner]bool{}
	add := func(o Owner, birth string) {
		if o.PID <= 1 {
			return
		}
		dead := ProbeOwner(o) == Dead
		if birth != "" {
			dead = processowner.ProvedDead(o.PID, birth)
		}
		if !dead {
			participants[o] = true
		}
	}
	for _, h := range s.Pool.Holders {
		add(h.Owner, "")
	}
	for _, h := range s.Pool.Waiting {
		add(h.Owner, "")
	}
	for _, p := range s.ProviderPools {
		for _, h := range p.Holders {
			add(h.Owner, "")
		}
		for _, h := range p.Waiting {
			add(h.Owner, "")
		}
	}
	for _, p := range s.Launches {
		add(p.Owner, p.Birth)
	}
	for _, p := range s.Panes {
		add(p.Owner, p.Birth)
	}
	for _, p := range s.HostLimits {
		add(p.Owner, p.Birth)
	}
	for _, p := range s.PaneLimits {
		add(p.Owner, p.Birth)
	}
	if s.WorkflowSlots != nil {
		for _, p := range s.WorkflowSlots.Instances {
			add(p.Owner, p.Birth)
		}
	}
	blocked := []string{}
	for o := range participants {
		if s.Readers[readerKey(o)].Schema < 2 {
			blocked = append(blocked, readerKey(o))
		}
	}
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return fmt.Errorf("host_ledger: schema 2 upgrade unavailable; participants have not advertised schema 2 support: %s", strings.Join(blocked, ", "))
	}
	s.Version = 2
	s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "ledger-schema-upgrade", ID: "2"})
	return nil
}

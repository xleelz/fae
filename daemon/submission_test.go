package daemon

import (
	"strings"
	"testing"

	"github.com/xleelz/fae/workflow"
)

// FAE-CLI-011: an explicitly Normal submission names exactly one of dry run
// and commit-external; Local and unstated submissions name neither.
func TestSubmissionModeCompleteTruthTable(t *testing.T) {
	for _, normal := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			for _, live := range []bool{false, true} {
				want := normal && dry != live
				if !normal {
					want = !dry && !live
				}
				if got := SubmissionModeComplete(normal, dry, live); got != want {
					t.Errorf("normal=%v dry=%v live=%v: %v want %v", normal, dry, live, got, want)
				}
			}
		}
	}
}

// FAE-MODE-004: each missing live value is refused by name, alone.
func TestLiveSubmissionNamesEachMissingValue(t *testing.T) {
	doc := workflow.Document{ID: "ticketed"}
	c := LiveConfig{TicketWorkflow: &TicketWorkflowConfig{Workflows: []string{doc.ID}}}
	names := map[string]string{"ticket": "ticket room (--ticket-room)", "destination": "review destination branch (--review-destination-branch)", "push": "push namespace (--push-namespace)"}
	clear := map[string]func(*AdmitParams){
		"ticket":      func(p *AdmitParams) { p.TicketRoom = "" },
		"destination": func(p *AdmitParams) { p.Destination = "" },
		"push":        func(p *AdmitParams) { p.PushNamespace = "" },
	}
	for value, drop := range clear {
		p := AdmitParams{ID: "t", Mode: "Normal", CommitExternal: true, TicketRoom: "room", Destination: "main", PushNamespace: "fae/"}
		drop(&p)
		err := validateTicketAdmission(c, doc, p)
		if err == nil || !strings.Contains(err.Error(), names[value]) {
			t.Fatalf("%s: missing value not named: %v", value, err)
		}
		for other, name := range names {
			if other != value && strings.Contains(err.Error(), name) {
				t.Fatalf("%s: present value %s named: %v", value, other, err)
			}
		}
		if value != "ticket" {
			// Without a ticket workflow, admission names the review pins alone.
			missing := missingLiveValues(p, false)
			if len(missing) != 1 || missing[0] != names[value] {
				t.Fatalf("%s: %v", value, missing)
			}
		}
	}
	err := validateTicketAdmission(c, doc, AdmitParams{ID: "t", Mode: "Normal", CommitExternal: true})
	for _, name := range names {
		if err == nil || !strings.Contains(err.Error(), "missing "+name) {
			t.Fatalf("every missing value must be named separately: %v", err)
		}
	}
	if e := validateTicketAdmission(c, doc, AdmitParams{ID: "t", Mode: "Normal", DryRun: true}); e != nil {
		t.Fatal("dry run needs no live pins", e)
	}
}

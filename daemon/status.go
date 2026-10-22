package daemon

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/kernel"
)

// JobIdentity is the immutable admission response and current read identity.
// Workflow documents, artifact bodies and operator content are never included.
type JobIdentity struct {
	ExecutionKind    string `json:"execution_kind"`
	ID               string `json:"id"`
	Version          int64  `json:"version"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRevision int64  `json:"workflow_revision"`
	TargetID         string `json:"target_id"`
	TargetRevision   int64  `json:"target_revision"`
	State            string `json:"state"`
}

func identity(j controlplane.JobSnapshot) JobIdentity {
	var marker struct {
		Job struct {
			ExecutionKind string `json:"execution_kind"`
		} `json:"job"`
	}
	_ = json.Unmarshal(j.Payload, &marker)
	return JobIdentity{marker.Job.ExecutionKind, j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State}
}

type BoardItem struct {
	JobIdentity
	Priority int    `json:"priority"`
	RunID    string `json:"run_id"`
	Mode     string `json:"mode"`
	Reason   string `json:"reason,omitempty"`
	InFlight bool   `json:"in_flight"`
}
type statusSnapshot struct {
	Schedules         map[string]json.RawMessage `json:"-"`
	ScheduleList      json.RawMessage            `json:"-"`
	ControllerUpdated time.Time                  `json:"-"`
	ControllerHealth  json.RawMessage            `json:"-"`
	TicketOutbox      ticketStatus               `json:"ticket_outbox"`
	Maintenance       map[string]any             `json:"maintenance,omitempty"`
	Drive             map[string]any             `json:"drive"`
	Configuration     map[string]any             `json:"configuration"`
	Drained           bool                       `json:"drained"`
	Draining          bool                       `json:"draining"`
	Active            int                        `json:"active"`
	Counts            map[string]int             `json:"counts"`
	Tasks             []BoardItem                `json:"tasks"`
	Updated           time.Time                  `json:"updated"`
}

// publishStatus is called by the serialized writer. Consumers must not mutate
// the published rows or maps, and never wait for SQL or the writer mutex.
func (d *Daemon) publishStatus() {
	s := statusSnapshot{Draining: d.draining, Active: len(d.active) + len(d.loopActive) + len(d.prophetActive), Counts: map[string]int{}, Tasks: []BoardItem{}, Updated: time.Now().UTC()}
	if v := d.ticketStatus.Load(); v != nil {
		s.TicketOutbox = v.(ticketStatus)
	}
	if d.ticketActive.Load() {
		s.Active++
	}
	s.Maintenance = d.publicMaintenance()
	s.Drive = map[string]any{"alive": d.driveAlive.Load(), "rounds": d.driveRounds.Load(), "last_transition": time.Unix(0, d.lastTransition.Load()).UTC()}
	s.Drive["generation"] = d.driveGeneration.Load()
	s.Drive["cause"], _ = d.driveCause.Load().(string)
	s.Configuration = map[string]any{"fixed": map[string]any{"instance_root": d.layout.Root, "socket_path": d.layout.Socket}}
	if d.live != nil {
		s.Configuration["dynamic"] = d.dynamicConfiguration()
	}
	pending := 0
	if d.backlogUncertain {
		pending++
	}
	for _, s := range d.loops {
		if s.InFlight {
			pending++
		}
	}
	for _, j := range d.jobs {
		var e envelope
		if decodeEnvelope(j.Payload, &e) != nil {
			continue
		}
		if e.Job.InFlight != nil {
			pending++
		}
		s.Counts[j.State]++
		s.Tasks = append(s.Tasks, BoardItem{identity(j), e.Admission.Priority, e.Admission.RunID, e.Admission.Mode, e.Job.Reason, e.Job.InFlight != nil})
	}
	sort.Slice(s.Tasks, func(i, j int) bool { return s.Tasks[i].ID < s.Tasks[j].ID })
	s.Drained = kernel.CanFinishDrain(d.draining, s.Active, pending+s.TicketOutbox.Uncertain)
	s.Schedules = map[string]json.RawMessage{}
	ids := []string{}
	for id, row := range d.loops {
		s.Schedules[id], _ = json.Marshal(row)
		ids = append(ids, id)
	}
	for id, row := range d.crons {
		s.Schedules[id], _ = json.Marshal(row)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []json.RawMessage{}
	for _, id := range ids {
		rows = append(rows, s.Schedules[id])
	}
	s.ScheduleList, _ = json.Marshal(rows)
	if d.child != nil {
		s.ControllerHealth, _ = json.Marshal(d.captureControllerHealth(false))
		s.ControllerUpdated = time.Now().UTC()
	}
	d.status.Store(s)
}

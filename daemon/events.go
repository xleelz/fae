package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"net/http"
	"strconv"
	"time"
)

func (d *Daemon) notifyEvents() {
	d.eventMu.Lock()
	close(d.eventWake)
	d.eventWake = make(chan struct{})
	d.eventMu.Unlock()
}

// Each subscriber has a bounded independent queue. A full queue terminates its
// producer and reports a gap with the last successfully sent cursor. It never
// blocks publication, drive, or another subscriber. A disconnected/unwritable
// client cannot receive a gap; it must reconnect with its last received cursor.
func (d *Daemon) stream(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		v, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || v < 0 {
			http.Error(w, "invalid sequence", 400)
			return
		}
		after = v
	}
	select {
	case d.subscribers <- struct{}{}:
		defer func() { <-d.subscribers }()
	default:
		http.Error(w, "subscriber limit", 429)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	queue := make(chan controlplane.Event, 100)
	gaps := make(chan EventGap, 1)
	failure := make(chan error, 1)
	filter := EventFilter{Task: r.URL.Query().Get("task"), TreeRoot: r.URL.Query().Get("tree_root"), RunID: r.URL.Query().Get("run_id"), WorkflowID: r.URL.Query().Get("workflow_id"), Kind: r.URL.Query().Get("kind")}
	go d.produceEventsFiltered(ctx, after, queue, gaps, failure, r.URL.Query().Get("include_retired") == "true", filter)
	w.Header().Set("Content-Type", "application/x-ndjson")
	control := http.NewResponseController(w)
	send := func(v any) bool {
		_ = control.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return json.NewEncoder(w).Encode(v) == nil && control.Flush() == nil
	}
	_ = control.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if control.Flush() != nil {
		return
	}
	for {
		// Once overflow occurs, discard unsent buffered events and report the cursor
		// already delivered. The client resumes from the plane, without silent loss.
		select {
		case gap := <-gaps:
			if gap.Reason == "overflow" {
				gap.ResumeAfter = after
			}
			send(gap)
			return
		default:
		}
		select {
		case gap := <-gaps:
			if gap.Reason == "overflow" {
				gap.ResumeAfter = after
			}
			send(gap)
			return
		case event := <-queue:
			if !send(event) {
				return
			}
			after = event.Sequence
		case <-failure:
			return
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		}
	}
}
func (d *Daemon) produceEvents(ctx context.Context, after int64, queue chan<- controlplane.Event, gaps chan<- EventGap, failure chan<- error, include ...bool) {
	d.produceEventsFiltered(ctx, after, queue, gaps, failure, len(include) > 0 && include[0], EventFilter{})
}
func (d *Daemon) produceEventsFiltered(ctx context.Context, after int64, queue chan<- controlplane.Event, gaps chan<- EventGap, failure chan<- error, include bool, filter EventFilter) {
	catchingUp := true
	for {
		d.eventMu.Lock()
		wake := d.eventWake
		d.eventMu.Unlock()
		query, cancel := context.WithTimeout(ctx, 5*time.Second)
		filterJSON, _ := json.Marshal(filter)
		value, err := d.readPool.run(query, "stream:"+strconv.FormatInt(after, 10)+":"+strconv.FormatBool(include)+":"+string(filterJSON), func(work context.Context) (any, error) {
			rows, cursor, more, gap, e := d.filteredEventBatch(work, after, 100, include, filter)
			return struct {
				Rows   []controlplane.Event
				Cursor int64
				More   bool
				Gap    *EventGap
			}{rows, cursor, more, gap}, e
		})
		cancel()
		if err != nil {
			failure <- err
			return
		}
		batch := value.(struct {
			Rows   []controlplane.Event
			Cursor int64
			More   bool
			Gap    *EventGap
		})
		if batch.Gap != nil {
			gaps <- *batch.Gap
			return
		}
		for _, event := range batch.Rows {
			if catchingUp {
				select {
				case <-ctx.Done():
					return
				case queue <- event:
					after = event.Sequence
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case queue <- event:
				after = event.Sequence
			default:
				gaps <- EventGap{Kind: "gap", Reason: "overflow", RequestedAfter: after, Timestamp: time.Now().UTC()}
				return
			}
		}
		after = batch.Cursor
		if batch.More {
			continue
		}
		catchingUp = false
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-wake:
		case <-time.After(time.Second):
		}
	}
}

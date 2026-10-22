package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"time"
)

type EventGap struct {
	Kind           string    `json:"kind"`
	Reason         string    `json:"reason"`
	RequestedAfter int64     `json:"requested_after"`
	ResumeAfter    int64     `json:"resume_after"`
	Timestamp      time.Time `json:"timestamp"`
}

func (d *Daemon) retainedEvents(ctx context.Context, after int64, limit int, include ...bool) ([]controlplane.Event, *EventGap, error) {
	if after < 0 || limit < 1 || limit > 100000 {
		return nil, nil, errors.New("invalid event range")
	}
	floor, err := d.store.EventFloor(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !CanResumeEvents(after, floor) {
		return nil, &EventGap{Kind: "gap", Reason: "retention", RequestedAfter: after, ResumeAfter: floor, Timestamp: time.Now().UTC()}, nil
	}
	var rows []controlplane.Event
	if len(include) > 0 && include[0] {
		rows, err = d.store.Events(ctx, after, limit)
	} else {
		rows, err = d.store.VisibleEvents(ctx, after, limit)
	}
	return rows, nil, err
}
func (d *Daemon) eventRead(ctx context.Context, after int64, limit int, include ...bool) (any, error) {
	rows, gap, err := d.retainedEvents(ctx, after, limit, include...)
	if err != nil {
		return nil, err
	}
	if gap != nil {
		raw, _ := json.Marshal(gap)
		return nil, &api.RPCError{Code: "event-gap", Message: string(raw)}
	}
	return rows, nil
}

func (d *Daemon) advanceEventWindow(ctx context.Context) error {
	age, count := d.config.EventRetentionAge, d.config.EventRetentionCount
	if age == 0 {
		age = 24 * time.Hour
	}
	if count == 0 {
		count = 100000
	}
	_, err := d.store.EventWindow(ctx, age, count, time.Now().UTC())
	return err
}
func (d *Daemon) maintainEventWindow(store *controlplane.Store) {
	defer d.workers.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
			age, count := d.config.EventRetentionAge, d.config.EventRetentionCount
			if age == 0 {
				age = 24 * time.Hour
			}
			if count == 0 {
				count = 100000
			}
			_, _ = store.EventWindow(ctx, age, count, time.Now().UTC())
			cancel()
		}
	}
}

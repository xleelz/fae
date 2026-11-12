package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
)

// Appendix A: host metrics ring buffer 15 min at 5 s. The route exhaustion
// alert uses the engine's route-exhaustion bound (routeExhaustionBound,
// default 30 min). Leak sampling shares the drive's 5 s round cadence.
const (
	hostSampleInterval    = 5 * time.Second
	hostSampleWindow      = 15 * time.Minute
	observeStepTimeout    = 5 * time.Second
	alertsFileName        = "alerts.log"
	observeErrorComponent = "observe"
)

// observer holds the daemon's observability state. Its zero value is ready;
// the function fields are test seams over the host.
type observer struct {
	round      sync.Mutex // one observation round at a time
	raise      sync.Mutex // serialises deduplicated raises
	mu         sync.Mutex
	ring       []HostSample
	next       int
	tree       processowner.Tree
	leaks      *processowner.LeakReport
	leakKeys   map[string]bool
	foreign    map[string]bool
	routeClock map[string]routeExhaustionClock
	failures   map[string]string
	mirror     alertMirror
	planeWake  chan struct{}
	planeAt    time.Time
	censusTree string
	censusAt   time.Time
	ownAt      time.Time
	// driveIdleAt is the drive's last idle verdict (FAE-ARC-002), 0 when busy.
	driveIdleAt atomic.Int64
	// idleRefusal is the open idle-maintenance refusal episode (idle.go).
	idleRefusal idleRefusal

	leaseWaits func(leaseWait)
	sample     func(context.Context) (hostguard.Sample, error)
	limits     processowner.LeakLimits
	// now dates the idle observability cadence; nil selects the system clock.
	now func() time.Time
}

func (o *observer) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// HostSample is one FAE-API-001 HostMetrics reading; nil means unreadable.
type HostSample struct {
	At              time.Time `json:"at"`
	MemoryAvailable *float64  `json:"memory_available_bytes"`
	SwapTotal       *float64  `json:"swap_total_bytes"`
	LoadPerCore     *float64  `json:"load_per_core"`
	IOFull10        *float64  `json:"io_full_avg10"`
	IOFull60        *float64  `json:"io_full_avg60"`
	MemoryFull10    *float64  `json:"memory_full_avg10"`
	MemoryFull60    *float64  `json:"memory_full_avg60"`
	DiskUsed        *float64  `json:"disk_used_fraction"`
	InodesUsed      *float64  `json:"inodes_used_fraction"`
	Error           string    `json:"error,omitempty"`
}

func reading(r hostguard.Reading) *float64 {
	if !r.Known {
		return nil
	}
	v := r.Value
	return &v
}

// maintainObservability runs every round: host sample, process-leak census,
// dead-edge and route-exhaustion alerts, and the alerts file mirror.
func (d *Daemon) maintainObservability() {
	defer d.workers.Done()
	ticker := time.NewTicker(hostSampleInterval)
	defer ticker.Stop()
	for {
		d.observeRound(d.ctx)
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (d *Daemon) observeRound(ctx context.Context) {
	d.obs.round.Lock()
	defer d.obs.round.Unlock()
	// FAE-ARC-002: while idle, plane steps wait for work or a write, and the
	// leak census waits for a change in the daemon's tree (observeIdle).
	idlePlane, leaks := d.observeIdle()
	for _, step := range []struct {
		name string
		run  func(context.Context) error
		skip bool
	}{{"host-sample", d.sampleHost, false}, {"host-alerts", d.ingestHostAlerts, false}, {"process-leaks", func(ctx context.Context) error { return d.detectLeaks(ctx, leaks == leakHostWide) }, leaks == leakSkip}, {"dead-edges", d.detectDeadEdges, idlePlane}, {"route-exhaustion", d.detectRouteExhaustion, idlePlane}, {"alerts-file", d.mirrorAlerts, idlePlane}} {
		if ctx.Err() != nil {
			return
		}
		if step.skip {
			continue
		}
		stepCtx, cancel := context.WithTimeout(ctx, observeStepTimeout)
		err := step.run(stepCtx)
		cancel()
		d.observeFailure(step.name, err)
	}
}

// observeFailure logs a step's failure once per distinct error and its recovery.
func (d *Daemon) observeFailure(step string, err error) {
	text := ""
	if err != nil {
		text = err.Error()
	}
	d.obs.mu.Lock()
	if d.obs.failures == nil {
		d.obs.failures = map[string]string{}
	}
	prior := d.obs.failures[step]
	d.obs.failures[step] = text
	d.obs.mu.Unlock()
	if prior == text || d.ctx.Err() != nil {
		return
	}
	if text == "" {
		d.logLine(observeErrorComponent, "observability step recovered step=%s\n", step)
		return
	}
	d.logLine(observeErrorComponent, "observability step failed step=%s error=%q\n", step, text)
}

func (d *Daemon) sampleHost(ctx context.Context) error {
	sample := d.obs.sample
	if sample == nil {
		sample = hostguard.Observe
	}
	h := HostSample{At: time.Now().UTC()}
	s, err := sample(ctx)
	if err != nil {
		h.Error = err.Error()
	} else {
		// A reading clear of the hard breaker ends the host-admission alert
		// episode that spooled broker refusals share (FAE-HOST-003).
		if d.live != nil && d.live.guard != nil && s.Memory.Known && !hostguard.Admit(s, d.live.guard.hostConfig(), 0, true).Hard {
			d.live.guard.hardAlerted.Store(false)
		}
		h.MemoryAvailable, h.SwapTotal, h.LoadPerCore = reading(s.Memory), reading(s.Swap), reading(s.LoadPerCore)
		h.IOFull10, h.IOFull60, h.MemoryFull10, h.MemoryFull60 = reading(s.IO10), reading(s.IO60), reading(s.Memory10), reading(s.Memory60)
	}
	var fs syscall.Statfs_t
	if e := syscall.Statfs(d.layout.Root, &fs); e == nil {
		if fs.Blocks > 0 {
			v := 1 - float64(fs.Bavail)/float64(fs.Blocks)
			h.DiskUsed = &v
		}
		if fs.Files > 0 {
			v := 1 - float64(fs.Ffree)/float64(fs.Files)
			h.InodesUsed = &v
		}
	}
	d.obs.mu.Lock()
	defer d.obs.mu.Unlock()
	size := int(hostSampleWindow / hostSampleInterval)
	if len(d.obs.ring) < size {
		d.obs.ring = append(d.obs.ring, h)
	} else {
		d.obs.ring[d.obs.next] = h
		d.obs.next = (d.obs.next + 1) % size
	}
	return err
}

// hostSamples returns the ring oldest first.
func (d *Daemon) hostSamples() []HostSample {
	d.obs.mu.Lock()
	defer d.obs.mu.Unlock()
	out := make([]HostSample, 0, len(d.obs.ring))
	out = append(out, d.obs.ring[d.obs.next:]...)
	return append(out, d.obs.ring[:d.obs.next]...)
}

//go:build linux

package daemon

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// ownListeners lists every listening socket held by this process's tree, with
// the holder: socket inodes from /proc/<pid>/fd matched against /proc/net.
func ownListeners(t *testing.T) map[string]int {
	t.Helper()
	children := map[int][]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
		ppid, _ := strconv.Atoi(f[1])
		children[ppid] = append(children[ppid], pid)
	}
	holder := map[string]int{}
	for queue := []int{os.Getpid()}; len(queue) > 0; queue = queue[1:] {
		pid := queue[0]
		queue = append(queue, children[pid]...)
		fds, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", pid))
		for _, fd := range fds {
			if target, err := os.Readlink(fd); err == nil && strings.HasPrefix(target, "socket:[") {
				holder[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = pid
			}
		}
	}
	found := map[string]int{}
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6", "unix"} {
		raw, err := os.ReadFile("/proc/net/" + table)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			f := strings.Fields(line)
			switch {
			case len(f) < 7:
			case table == "unix":
				flags, _ := strconv.ParseUint(f[3], 16, 32)
				if pid, ok := holder[f[6]]; ok && flags&0x10000 != 0 {
					path := ""
					if len(f) > 7 {
						path = f[7]
					}
					found["unix:"+path] = pid
				}
			case len(f) < 10:
			case strings.HasPrefix(table, "tcp") && f[3] == "0A" || strings.HasPrefix(table, "udp") && strings.Trim(strings.Split(f[2], ":")[0], "0") == "":
				if pid, ok := holder[f[9]]; ok {
					found[table[:3]+":"+procAddr(f[1])] = pid
				}
			}
		}
	}
	return found
}

func procAddr(field string) string {
	hostHex, portHex, _ := strings.Cut(field, ":")
	raw, _ := hex.DecodeString(hostHex)
	for i := 0; i+3 < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, _ := strconv.ParseUint(portHex, 16, 16)
	return net.JoinHostPort(net.IP(raw).String(), strconv.FormatUint(port, 10))
}

// FAE-SEC-001 with Herdr: a listener census of an instance running a live turn
// through Herdr finds only the authenticated RPC socket, Dolt's loopback port
// and Herdr's own API sockets, each inside a private 0700 session directory
// (the documented boundary: Herdr's protocol is unauthenticated).
func TestListenerCensusWithHerdr(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	herdr := testHerdr(t)
	cfg.Live.Adapter, cfg.Live.HerdrBinary = "multiplexer", herdr
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(d.layout.Root, "sessions") + "/"
	var mu sync.Mutex
	seen := map[string]int{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			for l, pid := range ownListeners(t) {
				mu.Lock()
				seen[l] = pid
				mu.Unlock()
				if path, ok := strings.CutPrefix(l, "unix:"); ok && strings.HasPrefix(path, sessions) {
					if info, err := os.Stat(filepath.Dir(path)); err == nil && info.Mode().Perm() != 0700 {
						mu.Lock()
						seen["open-directory:"+path] = pid
						mu.Unlock()
					}
				}
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	if _, err = call(t, d, "Admit", AdmitParams{ID: "census", AuthorizingMessage: "message"}, "census"); err != nil {
		t.Fatal(err)
	}
	awaitMultiplexerState(t, d, "census", "Done")
	close(done)
	wg.Wait()
	herdrSockets := 0
	for l, pid := range seen {
		switch path, unix := strings.CutPrefix(l, "unix:"); {
		case unix && path == d.layout.Socket:
		case unix && strings.HasPrefix(path, sessions):
			herdrSockets++
		case strings.HasPrefix(l, "tcp:127.0.0.1:") && pid == d.child.PID():
		default:
			t.Errorf("unexpected listener %s held by pid %d", l, pid)
		}
	}
	if herdrSockets == 0 {
		t.Fatalf("no Herdr socket observed during the live turn: %v", seen)
	}
}

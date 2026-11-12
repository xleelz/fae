//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processTree returns pid and every descendant with its kernel start time.
func processTree(t *testing.T, root int) map[int]string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	children, starts := map[int][]int{}, map[int]string{}
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
		starts[pid] = f[19]
	}
	tree := map[int]string{}
	for queue := []int{root}; len(queue) > 0; queue = queue[1:] {
		if _, seen := tree[queue[0]]; seen {
			continue
		}
		tree[queue[0]] = starts[queue[0]]
		queue = append(queue, children[queue[0]]...)
	}
	return tree
}

// procAddress decodes a /proc/net hex address (little-endian 32-bit words).
func procAddress(t *testing.T, field string) string {
	t.Helper()
	hostHex, portHex, _ := strings.Cut(field, ":")
	raw, err := hex.DecodeString(hostHex)
	if err != nil || len(raw)%4 != 0 {
		t.Fatalf("address %q", field)
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	port, _ := strconv.ParseUint(portHex, 16, 16)
	return net.JoinHostPort(net.IP(raw).String(), strconv.FormatUint(port, 10))
}

// listenerCensus lists every listening socket held by any process of the tree:
// socket inodes from /proc/<pid>/fd matched against /proc/net. TCP counts in
// LISTEN state, UDP when unconnected, Unix sockets with __SO_ACCEPTCON.
func listenerCensus(t *testing.T, tree map[int]string) []string {
	t.Helper()
	inodes := map[string]bool{}
	for pid := range tree {
		fds, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", pid))
		for _, fd := range fds {
			if target, err := os.Readlink(fd); err == nil && strings.HasPrefix(target, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
			}
		}
	}
	var found []string
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
				if inodes[f[6]] && flags&0x10000 != 0 {
					path := ""
					if len(f) > 7 {
						path = f[7]
					}
					found = append(found, "unix:"+path)
				}
			case len(f) < 10 || !inodes[f[9]]:
			case strings.HasPrefix(table, "tcp") && f[3] == "0A":
				found = append(found, "tcp:"+procAddress(t, f[1]))
			case strings.HasPrefix(table, "udp") && strings.Trim(strings.Split(f[2], ":")[0], "0") == "":
				found = append(found, "udp:"+procAddress(t, f[1]))
			}
		}
	}
	sort.Strings(found)
	return found
}

// FAE-SEC-001, FAE-ARC-006, FAE-API-030..033: a listener census of a running
// instance's whole process tree finds exactly the Unix RPC socket and Dolt's
// loopback port, plus the token-gated loopback UI port when the UI is
// enabled; SIGINT then stops the daemon and the UI child with no orphan.
func TestListenerCensusWithAndWithoutUI(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for the listener census")
	}
	for _, withUI := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-ui", true: "with-ui"}[withUI], func(t *testing.T) {
			root, err := os.MkdirTemp("/var/tmp", "fae-census-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			ws := filepath.Join(root, "ws")
			if err = os.Mkdir(ws, 0700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(ws)
			t.Setenv("CLI_TEST_AS_MAIN", "1")
			home := filepath.Join(root, "instances")
			fae := func(args ...string) ([]byte, error) {
				var out bytes.Buffer
				err := execute(context.Background(), append(args, "--home", home, "--instance", "census"), &out)
				return out.Bytes(), err
			}
			args := []string{"serve", "--workspace", ws, "--adapter", "scripted", "--dolt", binary}
			uiPort := 0
			if withUI {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				uiPort = l.Addr().(*net.TCPAddr).Port
				l.Close()
				args = append(args, "--ui", "--ui-bin", buildUIChild(t, root), "--ui-port", strconv.Itoa(uiPort))
			}
			raw, err := fae(args...)
			if err != nil {
				t.Fatal(err)
			}
			var line struct {
				PID           int
				Plane, Socket string
				UI            *string
			}
			if err = json.Unmarshal(raw, &line); err != nil || line.PID <= 1 {
				t.Fatalf("start line %s %v", raw, err)
			}
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_ = syscall.Kill(line.PID, syscall.SIGKILL)
				}
			})
			wantURL := fmt.Sprintf("http://127.0.0.1:%d/", uiPort)
			if withUI != (line.UI != nil) || withUI && *line.UI != wantURL {
				t.Fatalf("start line ui %v", line.UI)
			}
			var uiPID int
			if withUI {
				deadline := time.Now().Add(60 * time.Second)
				for {
					raw, err = fae("status")
					var status struct {
						UI struct {
							Health string
							PID    *int
						}
					}
					if err == nil && json.Unmarshal(raw, &status) == nil && status.UI.Health == "healthy" && status.UI.PID != nil {
						uiPID = *status.UI.PID
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("UI never healthy: %s %v", raw, err)
					}
					time.Sleep(50 * time.Millisecond)
				}
			}
			tree := processTree(t, line.PID)
			if withUI {
				if _, ok := tree[uiPID]; !ok {
					t.Fatalf("UI child %d outside the daemon's process tree %v", uiPID, tree)
				}
			}
			want := []string{"tcp:" + line.Plane, "unix:" + line.Socket}
			if withUI {
				want = append(want, fmt.Sprintf("tcp:127.0.0.1:%d", uiPort))
			}
			sort.Strings(want)
			if got := listenerCensus(t, tree); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("listener census\n got %v\nwant %v", got, want)
			}
			if withUI {
				// The UI port is token-gated: anonymous refused, the operator's token admitted.
				raw, err = fae("ui", "token")
				var token struct{ Token string }
				if err != nil || json.Unmarshal(raw, &token) != nil || len(token.Token) != 64 {
					t.Fatalf("ui token %s %v", raw, err)
				}
				client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
				for credential, code := range map[string]int{"": http.StatusUnauthorized, token.Token: http.StatusOK} {
					req, _ := http.NewRequest("GET", wantURL+"healthz", nil)
					if credential != "" {
						req.Header.Set("Authorization", "Bearer "+credential)
					}
					res, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if res.StatusCode != code {
						t.Fatalf("UI healthz with credential %t: %d", credential != "", res.StatusCode)
					}
				}
				serveLog, _ := os.ReadFile(filepath.Join(home, "census.serve.log"))
				supervision, _ := os.ReadFile(filepath.Join(home, "census", "supervision.log"))
				if bytes.Contains(serveLog, []byte(token.Token)) || bytes.Contains(supervision, []byte(token.Token)) {
					t.Fatal("UI token logged")
				}
			}
			// SIGINT stops the daemon; the UI child and every other owned process go too.
			if err = syscall.Kill(line.PID, syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Minute)
			for {
				survivors := []int{}
				for pid, start := range tree {
					raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
					if err != nil {
						continue
					}
					f := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+1:]))
					if f[19] == start && f[0] != "Z" {
						survivors = append(survivors, pid)
					}
				}
				if len(survivors) == 0 {
					break
				}
				if time.Now().After(deadline) {
					out, _ := exec.Command("ps", "-o", "pid,ppid,stat,args", "-p", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(survivors)), ","), "[]")).CombinedOutput()
					t.Fatalf("processes survived SIGINT: %v\n%s", survivors, out)
				}
				time.Sleep(50 * time.Millisecond)
			}
			stopped = true
		})
	}
}

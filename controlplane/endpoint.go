package controlplane

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EndpointError refuses a Dolt endpoint that something already serves
// (FAE-CP-004). Own is an unrecorded server on this instance's own data
// directory; otherwise the listener is foreign (or could not be identified).
// Each names its remedy.
type EndpointError struct {
	Port    int
	PID     int
	Command string
	Own     bool
	DataDir string
}

func (e *EndpointError) Error() string {
	at := fmt.Sprintf("127.0.0.1:%d", e.Port)
	switch {
	case e.Own:
		return fmt.Sprintf("Dolt endpoint %s is served by Dolt pid %d on this instance's data directory %s, which the instance pid file does not record; refusing adoption. Remedy: stop that unrecorded server (kill %d) once no other daemon uses it, then start again", at, e.PID, e.DataDir, e.PID)
	case e.PID > 0:
		return fmt.Sprintf("Dolt endpoint %s is served by another process (pid %d: %s), not data directory %s; refusing it. Remedy: stop that process, or pin a free port with serve --dolt-port N", at, e.PID, e.Command, e.DataDir)
	default:
		return fmt.Sprintf("Dolt endpoint %s is occupied by an unidentified listener; refusing adoption. Remedy: identify it (ss -ltnp 'sport = :%d'), stop it, or pin a free port with serve --dolt-port N", at, e.Port)
	}
}

// identifyListener names the local process listening on the loopback port,
// from /proc (Linux); elsewhere, or without permission, it names none.
func identifyListener(port int, dataDir string) *EndpointError {
	e := &EndpointError{Port: port, DataDir: dataDir}
	inode := listeningInode(port)
	if inode == "" {
		return e
	}
	procs, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range procs {
		if target, err := os.Readlink(fd); err != nil || target != "socket:["+inode+"]" {
			continue
		}
		dir := filepath.Dir(filepath.Dir(fd))
		e.PID, _ = strconv.Atoi(filepath.Base(dir))
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		e.Command = strings.Join(args, " ")
		cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
		e.Own = len(args) > 1 && filepath.Base(args[0]) == "dolt" && args[1] == "sql-server" && cwd == dataDir
		return e
	}
	return e
}

// listeningInode is the socket inode of a TCP listener on the port.
func listeningInode(port int) string {
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(table)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			// local_address is HEXIP:HEXPORT; state 0A is LISTEN.
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			_, hexPort, ok := strings.Cut(fields[1], ":")
			if p, err := strconv.ParseInt(hexPort, 16, 32); ok && err == nil && int(p) == port {
				f.Close()
				return fields[9]
			}
		}
		f.Close()
	}
	return ""
}

package tui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// Main runs fae-tui with its command-line arguments and returns the exit
// code: 0 on quit, 1 on a terminal failure, 2 on a usage error.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := ParseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "fae-tui:", err)
		return 2
	}
	socket, err := client.NewSocket(opts.Home, opts.Instance)
	if err != nil {
		fmt.Fprintln(stderr, "fae-tui:", err)
		return 2
	}
	program := tea.NewProgram(New(opts, client.New(socket)), tea.WithInput(stdin), tea.WithOutput(stdout), tea.WithContext(context.Background()))
	if _, err = program.Run(); err != nil {
		fmt.Fprintln(stderr, "fae-tui:", err)
		return 1
	}
	return 0
}

// ParseFlags reads --home, --instance, --write and --refresh. An empty
// --home or --instance keeps its default, as the fae CLI does.
func ParseFlags(args []string, stderr io.Writer) (Options, error) {
	f := flag.NewFlagSet("fae-tui", flag.ContinueOnError)
	f.SetOutput(stderr)
	o := Options{}
	f.StringVar(&o.Home, "home", "", "instance home (default ~/.fae/instances)")
	f.StringVar(&o.Instance, "instance", client.DefaultInstance, "instance name")
	f.BoolVar(&o.Write, "write", false, "allow creating, editing and deleting tasks and edges (default read-only)")
	f.DurationVar(&o.Refresh, "refresh", 2*time.Second, "base polling interval (250ms to 10m); each poll reads what the view shows, and polls slow down while nothing changes")
	f.Usage = func() {
		fmt.Fprintln(stderr, "usage: fae-tui [--home DIR] [--instance NAME] [--write] [--refresh DURATION]")
		fmt.Fprintln(stderr, "A terminal UI for a running Fae daemon: molecules, the task graph, tasks and what needs attention.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if strings.TrimSpace(o.Instance) == "" {
		o.Instance = client.DefaultInstance
	}
	if strings.TrimSpace(o.Home) == "" {
		home, err := client.DefaultHome()
		if err != nil {
			return o, fmt.Errorf("no --home and no user home directory: %w", err)
		}
		o.Home = home
	}
	if o.Refresh < 250*time.Millisecond || o.Refresh > 10*time.Minute {
		return o, fmt.Errorf("--refresh %s is outside 250ms..10m", o.Refresh)
	}
	if _, _, err := client.Paths(o.Home, o.Instance); err != nil {
		return o, err
	}
	return o, nil
}

// Command fae-tui is a terminal UI for a running Fae daemon. It ships beside,
// never inside, the core fae binary (FAE-GEN-010 non-goal 6) and reaches the
// daemon only over the instance RPC socket.
package main

import (
	"os"

	"github.com/xleelz/fae/tui"
)

func main() { os.Exit(tui.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

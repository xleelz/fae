// Command fae-ui is Fae's reference operator UI. The daemon supervises it as
// its optional UI child (fae serve --ui); it ships beside, never inside, the
// core fae binary (FAE-GEN-010 non-goal 6).
package main

import (
	"os"

	"github.com/xleelz/fae/ui/server"
)

func main() { os.Exit(server.Main(os.Args[1:], os.Stderr)) }

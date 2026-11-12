package effects

import "github.com/xleelz/fae/scratchfs"

// removeRunScratchTree removes a run's scratch or policy directory; tests
// replace it to make a removal fail.
var removeRunScratchTree = scratchfs.RemoveAll

// removeRunScratch removes a run's scratch or policy directory on every exit
// path (FAE-HOST-020), even when the run left read-only directories in it
// (Go's module cache under a test's own GOPATH, or an unsealed GOROOT
// mirror): scratchfs.RemoveAll makes the tree's directories writable without
// following links out of it. A directory it still cannot remove is reported
// to ScratchLeft rather than dropped.
func (b *Broker) removeRunScratch(dir string) {
	if err := removeRunScratchTree(dir); err != nil && b.ScratchLeft != nil {
		b.ScratchLeft(dir, err)
	}
}

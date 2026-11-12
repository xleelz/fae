package daemon

import "github.com/xleelz/fae/workflow"

func (d *Daemon) recursionCeiling() int {
	if d.config.RecursionCeiling > 0 {
		return d.config.RecursionCeiling
	}
	return workflow.DefaultRecursionCeiling
}
func (d *Daemon) parseWorkflow(raw []byte) (workflow.Document, error) {
	return workflow.ParseWithRecursionCeiling(raw, d.recursionCeiling())
}
func (d *Daemon) parseWorkflowWrite(raw []byte) (workflow.Document, error) {
	return workflow.ParseWriteWithRecursionCeiling(raw, d.recursionCeiling())
}
func (d *Daemon) validWorkflowWrite(raw []byte) error {
	_, err := d.parseWorkflowWrite(raw)
	return err
}

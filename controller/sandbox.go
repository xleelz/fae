package controller

import "fmt"

// SandboxRefusal is the FAE-HOST-030 refusal of a sandboxed launch: it names
// the inherited condition and the supported unsandboxed launchers.
func SandboxRefusal(component string, pid int, probe error) error {
	condition := sandboxCondition(pid)
	if probe != nil {
		condition = "sandbox state unreadable: " + probe.Error()
	}
	return fmt.Errorf("%s refused: it would inherit an agent sandbox (%s); start fae serve from an unsandboxed operator shell or a dedicated multiplexer pane (never from an agent session), and restart it through `fae runtime-launcher` with `fae maintenance start` or `fae roll`", component, condition)
}

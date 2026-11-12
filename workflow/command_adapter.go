package workflow

import "sort"

// validateCommandAdapters opts an exact operator-approved command into generic
// execution without weakening test-shape or formal-evidence semantics.
func validateCommandAdapters(d Document, bad func(string, string)) {
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		stage := d.Stages[id]
		path := "stages." + id + ".command_adapter"
		if stage.CommandAdapter != "" && stage.CommandAdapter != "builtin" && stage.CommandAdapter != "configured" {
			bad(path, "command_adapter must be builtin or configured")
			continue
		}
		if stage.CommandAdapter != "" && stage.Primitive != HostCommand {
			bad(path, "command_adapter belongs only to HostCommand")
		}
		if stage.Formal != nil && stage.CommandAdapter == "builtin" {
			bad(path, "formal commands require the configured adapter, implicitly or explicitly")
		}
		if stage.CommandAdapter == "configured" && (stage.TestRun || stage.TestShape != "") && !testShapeValid(stage) {
			bad(path, "configured test commands require an existing supported test_shape matching their command; the adapter does not create test evidence")
		}
	}
}

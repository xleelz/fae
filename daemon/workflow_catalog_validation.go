package daemon

import (
	"fmt"
	"github.com/xleelz/fae/workflow"
)

// ValidateWorkflowCatalog completes offline authoring validation against exact
// child pins. No running instance or credential is needed.
func ValidateWorkflowCatalog(raw []byte, catalog map[string]workflow.Document) WorkflowValidation {
	result := ValidateWorkflowContent(raw)
	if !result.Valid {
		return result
	}
	doc, err := workflow.ParseWrite(raw)
	if err != nil {
		return result
	}
	resolve := func(ref workflow.WorkflowRef) (workflow.Document, error) {
		key := fmt.Sprintf("%s@%d", ref.ID, ref.Revision)
		child, ok := catalog[key]
		if !ok && ref.Revision == 1 {
			child, ok = catalog[ref.ID]
		}
		if !ok || child.ID != ref.ID {
			return workflow.Document{}, fmt.Errorf("footprint child %s missing from --catalog", key)
		}
		return child, nil
	}
	issues := workflow.ValidateCatalog(doc, resolve)
	if len(issues) > 0 {
		result.Valid = false
		result.Footprint = nil
		result.Issues = append(result.Issues, issues...)
		return result
	}
	f, err := workflow.DeriveFootprint(doc, resolve, 0)
	if err != nil {
		result.Valid = false
		result.Footprint = nil
		result.FootprintError = err.Error()
		result.Issues = append(result.Issues, workflow.Issue{Path: "footprint", Message: err.Error()})
	} else {
		result.Footprint = &f
		result.FootprintError = ""
	}
	return result
}

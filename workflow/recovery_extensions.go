package workflow

import "strings"

func validateRecoveryExtensions(d Document, out map[string][]string, bad func(string, string)) {
	for id, s := range d.Stages {
		p := "stages." + id
		if s.SchemaRetries != nil && (s.Primitive != AgentTurn || *s.SchemaRetries < 0 || *s.SchemaRetries > 16) {
			bad(p+".schema_retries", "an AgentTurn retry bound in 0..16 required")
		}
		if s.OperatorMaySupply && s.Primitive != AgentTurn {
			bad(p+".operator_may_supply", "belongs to AgentTurn")
		}
		if s.OperatorMaySupply && ProducesChanges(d, s) {
			bad(p+".operator_may_supply", "cannot supply an artifact for a change producer or writable stage")
		}
		if s.OnChildEscalation != "" && (s.Primitive != SubWorkflow || s.OnChildEscalation != "wait") {
			bad(p+".on_child_escalation", "only wait on a SubWorkflow is supported")
		}
		for _, in := range s.Inputs {
			if in.Type == "failure" || strings.HasPrefix(in.Stage, "failure:") {
				allowed := false
				for failed := range d.Stages {
					if (in.Stage == "" || in.Stage == "failure:"+failed) && FailureHandlerOnly(d, failed, id) {
						allowed = true
					}
				}
				if !allowed {
					bad(p+".inputs", "failure evidence may be bound only inside its failure route")
				}
			}
		}
		if s.Primitive == ChangeRequest {
			if s.Target == "" || s.Artifact == "" || s.Value == nil {
				bad(p, "ChangeRequest requires target, artifact and a bound value projection")
			}
		}
		if s.ItemsFrom != nil && s.Primitive != SubWorkflow {
			if s.Primitive != HumanDecision || len(s.ItemOptions) < 1 || len(s.ItemOptions) > 8 {
				bad(p+".item_options", "HumanDecision items require 1..8 item options")
			}
			seen := map[string]bool{}
			for _, o := range s.ItemOptions {
				if !identifier(o.ID, 64) || seen[o.ID] {
					bad(p+".item_options", "item option IDs must be unique")
				}
				seen[o.ID] = true
			}
		}
		if len(s.ItemOptions) > 0 && (s.ItemsFrom == nil || s.Primitive != HumanDecision) {
			bad(p+".items_from", "required with item_options")
		}
	}
}

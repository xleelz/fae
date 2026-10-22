package daemon

import (
	"context"
	"fmt"
	"github.com/xleelz/fae/external"
	"strings"
)

// ValidateExternalSchemas is a startup floor for every installed effect alias.
func ValidateExternalSchemas(ctx context.Context, c LiveConfig) error {
	d := &external.Discovery{}
	for alias, t := range c.ExternalTools {
		dynamic := map[string]string{}
		if c.TicketWorkflow != nil {
			for _, a := range c.TicketWorkflow.Actions {
				if a.Tool == alias {
					for key := range a.Bindings {
						dynamic[key] = "string"
					}
				}
			}
		}
		managed := false
		for _, p := range c.ReviewPolicies {
			if p.Operations[alias] != "" || p.StatusTool == alias || p.BaselineTool == alias || reviewOperation(t.Name) {
				managed = true
			}
		}
		if managed {
			for key, value := range t.Arguments {
				if !strings.HasPrefix(value, "$") {
					continue
				}
				typ := "string"
				switch value {
				case "$review", "$workspace", "$branch", "$package", "$integration_tip":
				case "$revision":
					typ = "integer"
				default:
					return fmt.Errorf("external alias %s unknown daemon token %s", alias, value)
				}
				if prior, ok := dynamic[key]; ok && prior != typ {
					return fmt.Errorf("external alias %s conflicting binding type for %s", alias, key)
				}
				dynamic[key] = typ
			}
		}
		schema, err := d.DiscoverTemplate(ctx, t, dynamic)
		if err != nil {
			return fmt.Errorf("external alias %s schema: %w", alias, err)
		}
		t.DiscoveredSchema = &schema
		c.ExternalTools[alias] = t
	}
	return validateFleetProvisionTools(c)
}

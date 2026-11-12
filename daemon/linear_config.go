package daemon

import (
	"fmt"
	"github.com/xleelz/fae/symphony"
)

func validateEmbeddedLinear(cfg symphony.ServiceConfig) error {
	if err := symphony.ValidateFaeConfig(cfg.Fae); err != nil {
		return err
	}
	for _, hook := range []*string{cfg.Hooks.AfterCreate, cfg.Hooks.BeforeRun, cfg.Hooks.AfterRun, cfg.Hooks.BeforeRemove} {
		if hook != nil && *hook != "" {
			return fmt.Errorf("embedded Fae uses its engine worktrees; Symphony workspace hooks require standalone fae symphony")
		}
	}
	if len(cfg.Worker.SSHHosts) > 0 {
		return fmt.Errorf("embedded Fae uses its configured runtime; Symphony SSH workers require standalone fae symphony")
	}
	return nil
}

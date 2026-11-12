package main

import (
	"context"
	"io"
	"path/filepath"
	"strings"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

const importLegacyUsage = "--source PATH|--from PATH [--dry-run] [--authorizing-message ID]"

// importLegacyCommand implements `fae import-legacy --source <path> [--dry-run]` (FAE-CLI-051).
func importLegacyCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("import-legacy", c)
	source := f.String("source", "", "legacy task export file path (JSON object, JSON array, or JSONL)")
	from := f.String("from", "", "legacy task export file path (alias of --source)")
	dryRun := f.Bool("dry-run", false, "validate and preview legacy tasks without writing to Dolt")
	if err := parse(f, args); err != nil {
		return err
	}
	rawSource := strings.TrimSpace(*source)
	rawFrom := strings.TrimSpace(*from)
	if rawSource != "" && rawFrom != "" && rawSource != rawFrom {
		return usage("import-legacy takes --source or --from, not two different paths")
	}
	rawPath := rawSource
	if rawPath == "" {
		rawPath = rawFrom
	}
	if rawPath == "" {
		return usage("import-legacy requires --source PATH (or --from PATH) (usage: fae import-legacy %s)", importLegacyUsage)
	}
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return err
	}
	tasks, err := daemon.ParseLegacyTasksFile(absPath)
	if err != nil {
		return err
	}
	params := daemon.ImportLegacyParams{
		From:               absPath,
		Source:             absPath,
		DryRun:             *dryRun,
		AuthorizingMessage: opts.message,
	}
	err = rpc(ctx, *opts, api.MethodImportLegacy, "", params, out)
	if err != nil && exitCode(err) == 3 {
		if *dryRun {
			taskIDs := make([]string, 0, len(tasks))
			for _, t := range tasks {
				taskIDs = append(taskIDs, t.ID)
			}
			return jsonOut(out, daemon.ImportLegacyResult{
				DryRun:   true,
				Count:    len(tasks),
				Imported: 0,
				Skipped:  0,
				TaskIDs:  taskIDs,
				Tasks:    tasks,
			})
		}
		res, offlineErr := daemon.ImportLegacyOffline(ctx, opts.home, opts.instance, params)
		if offlineErr != nil {
			return offlineErr
		}
		return jsonOut(out, res)
	}
	return err
}

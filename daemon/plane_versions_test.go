package daemon

import (
	"regexp"
	"strings"
	"testing"
)

// FAE-CP-008: after a drive of three tasks, every commit of the plane's
// history names its verb or drive round and its actor, and the diff of a
// round's checkpoint commit holds exactly that checkpoint's task rows.
func TestDriveOfThreeTasksVersionedPerRoundAndVerb(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	tasks := []string{"versioned-a", "versioned-b", "versioned-c"}
	for _, id := range tasks {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range tasks {
		awaitState(t, d, id, "Done")
	}
	commits, err := d.store.Commits(ctx, 10000)
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`^.+ by [a-z][a-z-]*$`)
	round := regexp.MustCompile(`^drive round (\d+) checkpoint (\S+) by daemon$`)
	checkpoints := map[string]int{}
	admits, migrations := 0, 0
	for _, c := range commits {
		if c.Message == "Initialize data repository" {
			continue
		}
		if !named.MatchString(c.Message) {
			t.Errorf("commit %s names no verb or round and actor: %q", c.Hash, c.Message)
		}
		switch m := round.FindStringSubmatch(c.Message); {
		case strings.HasPrefix(c.Message, "Admit by "):
			admits++
		case strings.HasPrefix(c.Message, "migration "):
			migrations++
		case m != nil:
			checkpoints[m[2]]++
			for table, column := range map[string]string{"jobs": "job_id", "transitions": "task_id", "tasks": "task_id"} {
				keys, err := d.store.ChangedKeys(ctx, c.Hash, table, column)
				if err != nil {
					t.Fatal(err)
				}
				if len(keys) > 1 || len(keys) == 1 && keys[0] != m[2] {
					t.Fatalf("round commit %q changed %s rows of %v", c.Message, table, keys)
				}
			}
		}
	}
	if admits != len(tasks) || migrations == 0 {
		t.Fatalf("admits %d migrations %d", admits, migrations)
	}
	for _, id := range tasks {
		if checkpoints[id] < 2 {
			t.Fatalf("task %s has %d round commits: %v", id, checkpoints[id], checkpoints)
		}
	}
	t.Logf("%d commits, %d round checkpoints", len(commits), len(checkpoints))
}

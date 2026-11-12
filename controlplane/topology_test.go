package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedPlane plants ended molecules with Done tasks, running molecules with
// live tasks, and transitions spread over every task, by direct SQL.
func seedPlane(t testing.TB, s *Store, ended, running, tasksPer, transitions int) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		if _, e := s.db.ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now().UTC()
	tasks := []string{}
	molecules, taskRows := [][]any{}, [][]any{}
	for m := 0; m < ended+running; m++ {
		id := Digest([]byte(fmt.Sprint("molecule", m)))
		var end any
		state := "spec"
		if m < ended {
			end, state = now, "Done"
		}
		molecules = append(molecules, []any{id, fmt.Sprint("wf", m%7), m + 1, id, now, end})
		for n := 0; n < tasksPer; n++ {
			task := fmt.Sprintf("task-%d-%d", m, n)
			tasks = append(tasks, task)
			taskRows = append(taskRows, []any{task, "run", id, "target", 1, state, 1, 20, 10, `{"mode":"Local"}`, now, now})
		}
	}
	insert := func(head string, rows [][]any) {
		for start := 0; start < len(rows); start += 1000 {
			values, args := []string{}, []any{}
			for _, row := range rows[start:min(start+1000, len(rows))] {
				values = append(values, "("+strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")+")")
				args = append(args, row...)
			}
			exec(head+strings.Join(values, ","), args...)
		}
	}
	insert(`INSERT INTO molecules (molecule_id,workflow_id,revision,content_digest,created_at,ended_at) VALUES `, molecules)
	insert(`INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,created_at,updated_at) VALUES `, taskRows)
	// Transitions are generated in the plane, one statement however many: the
	// k-th names task k mod len(tasks), as task-<m>-<n> does.
	digit := `(SELECT 0 i UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)`
	from, k := []string{}, []string{}
	for scale := 1; scale < transitions; scale *= 10 {
		name := fmt.Sprintf("d%d", len(from))
		from = append(from, digit+" "+name)
		k = append(k, fmt.Sprintf("%d*%s.i", scale, name))
	}
	exec(`INSERT INTO transitions (task_id,from_state,to_state,owner,attempt,evidence_digest,note,message_id,created_at) SELECT CONCAT('task-',FLOOR(MOD(k,?)/?),'-',MOD(MOD(k,?),?)),'spec','plan','spec',1,?,'seeded','',? FROM (SELECT `+strings.Join(k, "+")+` AS k FROM `+strings.Join(from, ",")+`) g WHERE k<?`, len(tasks), tasksPer, len(tasks), tasksPer, strings.Repeat("e", 64), now, transitions)
}

// topologyRead pages every running molecule, returning the slowest page.
func topologyRead(t testing.TB, s *Store, running, tasksPer int) time.Duration {
	t.Helper()
	slowest, cursor, molecules := time.Duration(0), "", 0
	for {
		started := time.Now()
		page, e := s.Topology(context.Background(), "", cursor, 100, 10000, false)
		elapsed := time.Since(started)
		if e != nil {
			t.Fatal(e)
		}
		slowest = max(slowest, elapsed)
		if page.Statements != 2 && len(page.Molecules) > 0 || page.Rows > 101+100*tasksPer {
			t.Fatalf("topology read unscoped: %d statements, %d rows", page.Statements, page.Rows)
		}
		for _, m := range page.Molecules {
			if m.EndedAt != nil || len(m.Tasks) != tasksPer {
				t.Fatalf("topology page %+v", m)
			}
		}
		molecules += len(page.Molecules)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if molecules != running {
		t.Fatalf("topology read %d running molecules, want %d", molecules, running)
	}
	return slowest
}

// FAE-CP-009 acceptance: over a synthetic plane of 100k transition rows the
// topology read touches only the molecule and task tables in two keyed
// statements per page, within the 200 ms budget. It runs whenever the Dolt
// integration runs; its slowest page is logged as the recorded result.
func TestTopologyScopedOverLargePlane(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	seedPlane(t, s, 1000, 50, 20, 100000)
	var n int
	if e := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM transitions`).Scan(&n); e != nil || n != 100000 {
		t.Fatal("seeded transitions", n, e)
	}
	// Plant Retired tasks on a running molecule too; the default read must
	// neither read nor count them.
	now := time.Now().UTC()
	molID := Digest([]byte("molecule1000"))
	if _, e := s.db.ExecContext(context.Background(), `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,created_at,updated_at) VALUES ('retired-on-running','run',?,'target',1,'Retired',1,20,10,'{"mode":"Local"}',?,?)`, molID, now, now); e != nil {
		t.Fatal(e)
	}
	elapsed := topologyRead(t, s, 50, 20)
	t.Logf("FAE-CP-009 topology: slowest page %s over 100000 transitions, 1050 molecules, 21000 tasks", elapsed)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("topology page took %s, budget 200ms", elapsed)
	}
}

// BenchmarkTopology100kTransitions is the FAE-CP-009 acceptance plane: 100k
// transition rows. Each iteration pages every running molecule; a page over
// the 200 ms budget fails. Run with FAE_TEST_DOLT_BINARY set:
// go test -run '^$' -bench Topology100k ./controlplane
func BenchmarkTopology100kTransitions(b *testing.B) {
	s := testPlane(b, SchemaVersion)
	seedPlane(b, s, 1000, 50, 20, 100000)
	pages := []time.Duration{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pages = append(pages, topologyRead(b, s, 50, 20))
	}
	b.StopTimer()
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	b.ReportMetric(float64(pages[len(pages)/2].Microseconds())/1000, "ms/slowest-page")
	if pages[len(pages)/2] > 200*time.Millisecond {
		b.Fatalf("median slowest topology page %s exceeds the 200ms budget", pages[len(pages)/2])
	}
}

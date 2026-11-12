package daemon

import (
	"context"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestTreeSessionAdmissionIsolationReductionAndPriorityFIFO(t *testing.T) {
	var g treeSessionGate
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r1, err := g.acquire(ctx, "one", "a", 10, time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r1()
	r2, err := g.acquire(ctx, "one", "b", 10, time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	other, err := g.acquire(ctx, "other", "x", 10, time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	other()
	g.set("one", 1)
	granted := make(chan string, 2)
	start := func(id string, priority int) {
		go func() {
			release, err := g.acquire(ctx, "one", id, priority, time.Time{}, 2)
			if err == nil {
				granted <- id
				release()
			}
		}()
	}
	start("low", 10)
	start("high", 20)
	for {
		g.mu.Lock()
		n := len(g.waiters)
		g.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	r2()
	select {
	case x := <-granted:
		t.Fatal("limit reduction ignored", x)
	case <-time.After(20 * time.Millisecond):
	}
	r1()
	if x := <-granted; x != "high" {
		t.Fatal("priority FIFO violated", x)
	}
	if x := <-granted; x != "low" {
		t.Fatal("waiter lost", x)
	}
}
func TestScopedRoleKeysShareTreeAndIsolateTask(t *testing.T) {
	a := &liveAdmission{}
	a.view.Store(&scheduleView{treeRoot: map[string]string{"a": "root", "b": "root", "c": "other"}})
	doc := workflow.Document{ID: "workflow"}
	role := workflow.Role{Scope: "task-bound", Cap: 1, CapScope: "tree"}
	ka, _, _ := a.rolePool(doc, "a", "oracle", role)
	kb, _, _ := a.rolePool(doc, "b", "oracle", role)
	kc, _, _ := a.rolePool(doc, "c", "oracle", role)
	if ka != kb || ka == kc {
		t.Fatal("tree scope incorrect")
	}
	role.CapScope = "task"
	ka, _, _ = a.rolePool(doc, "a", "oracle", role)
	kb, _, _ = a.rolePool(doc, "b", "oracle", role)
	if ka == kb {
		t.Fatal("task scope shared")
	}
	_ = engine.Job{}
}

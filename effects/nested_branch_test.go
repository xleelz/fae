package effects

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNestedBranchResumePreservesTurnsAndInnerJoin(t *testing.T) {
	b, parent, _ := setup(t)
	ctx := context.Background()
	outer, err := b.ForkBranchWorktree(ctx, parent, "outer/visit-1", "left", []string{"nested/**"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(outer.Binding.Path, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, outer.Binding.Path, "nested/before", "before")
	outer, err = b.CommitBranchWorktree(ctx, outer, nil, "before inner fan")
	if err != nil {
		t.Fatal(err)
	}
	// A second turn adopts the same committed ref rather than reforking from root.
	resumed, err := b.ForkBranchWorktree(ctx, parent, "outer/visit-1", "left", []string{"nested/**"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(resumed.Binding.Path, "nested/before")); err != nil {
		t.Fatal("earlier branch turn lost:", err)
	}
	var children []BranchWorktree
	for _, name := range []string{"a", "b"} {
		child, e := b.ForkBranchWorktree(ctx, resumed.Binding, "inner/visit-1", "left/"+name, []string{"nested/" + name})
		if e != nil {
			t.Fatal(e)
		}
		write(t, child.Binding.Path, "nested/"+name, name)
		child, e = b.CommitBranchWorktree(ctx, child, nil, "inner child")
		if e != nil {
			t.Fatal(e)
		}
		children = append(children, child)
	}
	if _, err = b.IntegrateBranchWorktrees(ctx, resumed.Binding, children); err != nil {
		t.Fatal(err)
	}
	// The enclosing branch sees both inner outputs before its next runtime turn.
	for _, name := range []string{"before", "a", "b"} {
		if _, err = os.Stat(filepath.Join(resumed.Binding.Path, "nested", name)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, resumed.Binding.Path, "nested/after", "after")
	aggregate, err := b.CommitBranchWorktree(ctx, resumed, nil, "after inner join")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.IntegrateBranchWorktrees(ctx, parent, []BranchWorktree{aggregate}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"before", "a", "b", "after"} {
		if _, err = os.Stat(filepath.Join(parent.Path, "nested", name)); err != nil {
			t.Fatal("root join lost nested write:", name, err)
		}
	}
	// A later visit must start independently of the earlier branch's ref.
	fresh, err := b.ForkBranchWorktree(ctx, parent, "outer/visit-2", "left", []string{"nested/**"})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Ref == aggregate.Ref {
		t.Fatal("fan visits share branch ref")
	}
}

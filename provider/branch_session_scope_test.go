package provider

import (
	"context"
	"testing"
)

func TestProviderContextIsScopedToBranchInstance(t *testing.T) {
	launch := persistentLaunch(t)
	root := sessionRoot(t)
	launch.BranchInstance = "root/fan/branch-0/visit-1"
	first, err := openProviderSession(context.Background(), root, launch)
	if err != nil {
		t.Fatal(err)
	}
	firstKey := first.state.Scope
	if err = first.complete(testThread); err != nil {
		t.Fatal(err)
	}
	first.close()
	launch.BranchInstance = "root/fan/branch-1/visit-1"
	sibling, err := openProviderSession(context.Background(), root, launch)
	if err != nil {
		t.Fatal(err)
	}
	if sibling.resumed || sibling.state.Scope == firstKey {
		t.Fatal("sibling inherited provider context")
	}
	sibling.close()
	launch.BranchInstance = "root/fan/branch-0/visit-1"
	retry, err := openProviderSession(context.Background(), root, launch)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.resumed || retry.state.ThreadID != testThread {
		t.Fatal("branch did not retain own context")
	}
	retry.close()
	launch.BranchInstance = "root/fan/branch-0/visit-2"
	next, err := openProviderSession(context.Background(), root, launch)
	if err != nil {
		t.Fatal(err)
	}
	if next.resumed || next.state.Scope == firstKey {
		t.Fatal("new fan instance inherited prior visit context")
	}
	next.close()
}

//go:build !linux

package commandshim

import (
	"context"
	"fmt"
)

type LeaseProxy struct{}

func StartLeaseProxy(context.Context, string, RoleShims) (*LeaseProxy, error) {
	return nil, fmt.Errorf("namespace heavy-operation lease proxy requires Linux peer credentials")
}
func (*LeaseProxy) BindProcess(int) {}
func (*LeaseProxy) Close() error    { return nil }
func runProxyCommand(context.Context, HeavyOp) (int, error) {
	return HeavyOpRefused, fmt.Errorf("namespace lease proxy requires Linux")
}

package zotigod

import (
	"context"

	"github.com/jayyao97/zotigo/internal/codexapp"
)

type codexSyncFuncRPC struct {
	call func(context.Context, string, any, any) error
}

func (r codexSyncFuncRPC) Call(ctx context.Context, method string, params any, result any) error {
	return r.call(ctx, method, params, result)
}

func (codexSyncFuncRPC) Notify(string, any) error { return nil }

type codexSyncHost struct {
	rpc      codexapp.RPC
	acquires int
	err      error
}

func (h *codexSyncHost) Acquire(context.Context) (*codexapp.Lease, error) {
	h.acquires++
	if h.err != nil {
		return nil, h.err
	}
	return &codexapp.Lease{RPC: h.rpc}, nil
}

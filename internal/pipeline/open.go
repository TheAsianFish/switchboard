package pipeline

import (
	"context"

	"github.com/TheAsianFish/switchboard/internal/core"
)

// Open exposes a single provider without authentication or routing. It is
// what `switchboard fake-provider` serves: any key and any model name are
// accepted and passed through unchanged.
type Open struct {
	Provider core.Provider
	// Names is what the models endpoint lists.
	Names []string
}

var _ core.Gateway = Open{}

// Authenticate implements core.Gateway and accepts any key.
func (o Open) Authenticate(ctx context.Context, _ string) (context.Context, error) {
	return ctx, nil
}

// Complete implements core.Gateway.
func (o Open) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	return o.Provider.Complete(ctx, req)
}

// Stream implements core.Gateway.
func (o Open) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	return o.Provider.Stream(ctx, req)
}

// Models implements core.Gateway.
func (o Open) Models(context.Context) []string { return o.Names }

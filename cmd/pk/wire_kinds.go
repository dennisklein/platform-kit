package main

import (
	"context"
	"errors"

	"github.com/dennisklein/platform-kit/internal/bench"
	"github.com/dennisklein/platform-kit/internal/config"
	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/policy"
	"github.com/dennisklein/platform-kit/internal/render"
)

// wireKinds sets the kind registry, the renderer and the policy engine (area B).
// The policy engine reads the policy from main through the change service,
// which wireChanges sets later; the closure looks it up at call time.
func wireKinds(ctx context.Context, cfg config.Config, d *bench.Deps) error {
	reg := all.Registry()
	d.Kinds = reg
	d.Renderer = render.New(reg)
	d.Policy = policy.NewEngine(func(ctx context.Context) (string, error) {
		if d.Changes == nil {
			return "", errors.New("policy: the change service is not wired")
		}
		return d.Changes.MainDir(ctx)
	})
	return nil
}

package talos_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

type closeCounter struct {
	talos.Client
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

func TestWith(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("boom")

	t.Run("closes after fn, returns fn's error", func(t *testing.T) {
		cc := &closeCounter{}
		dial := func(context.Context, talos.Target) (talos.Client, error) { return cc, nil }
		err := talos.With(ctx, dial, talos.Target{}, func(talos.Client) error {
			assert.Zero(t, cc.closed, "the client is open while fn runs")
			return boom
		})
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, 1, cc.closed)
	})

	t.Run("dial failure skips fn", func(t *testing.T) {
		dial := func(context.Context, talos.Target) (talos.Client, error) { return nil, boom }
		err := talos.With(ctx, dial, talos.Target{}, func(talos.Client) error {
			t.Fatal("fn must not run without a client")
			return nil
		})
		assert.ErrorIs(t, err, boom)
	})
}

package poll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUntilDone(t *testing.T) {
	var calls int
	err := Until(t.Context(), Every(time.Millisecond), func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls)
}

func TestUntilReturnsFnError(t *testing.T) {
	boom := errors.New("boom")
	err := Until(t.Context(), Every(time.Hour), func(context.Context) (bool, error) { return false, boom })
	assert.ErrorIs(t, err, boom, "fn's error ends the wait without a pause")
}

func TestUntilCancelEndsPause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var calls int
	start := time.Now()
	time.AfterFunc(20*time.Millisecond, cancel)
	err := Until(ctx, Every(time.Hour), func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
	assert.Less(t, time.Since(start), 5*time.Second, "cancellation must end the pause, not wait it out")
}

func TestBackoffGrowsToMax(t *testing.T) {
	b := Backoff{Initial: 50 * time.Millisecond, Max: 2 * time.Second, Factor: 2}
	var got []time.Duration
	d := b.Initial
	for range 8 {
		got = append(got, d)
		d = b.next(d)
	}
	assert.Equal(t, []time.Duration{
		50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond,
		800 * time.Millisecond, 1600 * time.Millisecond, 2 * time.Second, 2 * time.Second,
	}, got)
	assert.Equal(t, time.Second, Every(time.Second).next(time.Second), "Every keeps the pause constant")
}

func TestSleep(t *testing.T) {
	require.NoError(t, Sleep(t.Context(), time.Millisecond))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, Sleep(ctx, time.Hour), context.Canceled)
}

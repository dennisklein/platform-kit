package gofish

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// endless is a reader of spaces that never ends.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// TestBodySizeCap: a BMC that sends a 1 GiB body is refused after the cap,
// for a raw resource read and for a task monitor alike.
func TestBodySizeCap(t *testing.T) {
	const huge = "/redfish/v1/Huge"
	f, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	tree := sim.NewServer(sim.NewState(f))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != huge {
			tree.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, io.LimitReader(endless{}, 1<<30))
	}))
	defer srv.Close()
	bc, err := Connect(context.Background(), Config{Endpoint: srv.URL})
	require.NoError(t, err)
	defer bc.Close()
	c := bc.(*client)

	_, _, err = c.getRaw(huge)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "MiB"), "err = %v", err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = c.pollTask(ctx, huge)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MiB")
}

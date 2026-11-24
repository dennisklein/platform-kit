package gofish_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// A service that answers 202 with the Task in the body and no Location
// header, and mounts the medium as that task: the client follows the body's
// Task, so a mount that fails is an error and a mount that succeeds reads
// back as inserted when InsertMedia returns.
func TestConformanceWithTaskInBody(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "generic"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Protocol.TaskDuration = 300 * time.Millisecond
			f.Protocol.TaskInBody = true
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()
			bmctest.Run(t, c, f, false)
		})
	}
}

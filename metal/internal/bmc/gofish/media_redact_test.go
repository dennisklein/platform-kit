package gofish_test

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestInsertMediaErrorIsMasked: a mount the BMC refuses comes back with an
// error that names neither the grant token nor the grant password, on both
// dialects. The sim echoes the image URL as it was given, which on the vmm
// dialect (Huawei iBMC) carries the credentials as userinfo.
func TestInsertMediaErrorIsMasked(t *testing.T) {
	const (
		token    = "0123456789abcdef0123456789abcdef"
		password = "fedcba9876543210fedcba9876543210"
	)
	// A media address that refuses every connection: the mount fails at
	// the BMC's fetch.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	image := "https://" + addr + "/grants/" + token + "/node-config.iso"

	for _, name := range []string{"huawei-2288h-v5", "supermicro-x12"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()

			media, err := c.Media(context.Background())
			require.NoError(t, err)
			caps, err := c.Capabilities(context.Background())
			require.NoError(t, err)
			slots := bmc.InsertableSlots(media, caps)
			require.NotEmpty(t, slots)
			err = c.InsertMedia(context.Background(), slots[0], image,
				&bmc.MediaCredentials{Username: "media", Password: password})
			require.Error(t, err, "the BMC cannot fetch the image")
			assert.NotContains(t, err.Error(), token, "the grant token is masked")
			assert.NotContains(t, err.Error(), password, "the grant password is masked")
		})
	}
}

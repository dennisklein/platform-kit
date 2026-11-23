package gofish_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestConformanceOverHTTP is the core seam proof: the PRODUCTION client (gofish + raw OEM
// paths) passes the same bmctest suite as the in-process sim client, against the sim's
// HTTP tree — both sides of the seam exercised, for both vendor shapes.
func TestConformanceOverHTTP(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "supermicro-x12"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()

			c, err := gofishclient.Connect(context.Background(), gofishclient.Config{Endpoint: srv.URL})
			require.NoError(t, err)
			defer c.Close()
			bmctest.Run(t, c, f, false)
		})
	}
}

// TestConformanceOverHTTPWithAccounts runs the suite against an
// enforcement-enabled tree — the production client authenticating with the
// factory account the fixture seeds, the account lane active.
func TestConformanceOverHTTPWithAccounts(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "supermicro-x12"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Accounts = []sim.FixtureAccount{
				{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
			}
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()

			c, err := gofishclient.Connect(context.Background(),
				gofishclient.Config{Endpoint: srv.URL, Username: "admin", Password: "factory"})
			require.NoError(t, err)
			defer c.Close()
			bmctest.Run(t, c, f, false)
		})
	}
}

// TestAccountRotationOverHTTP pins the credential lifecycle across the wire:
// a rotated password kills the old credential on the very next call, the new
// one works, and a bad credential connects (the service root is public) but
// fails its first authenticated read.
func TestAccountRotationOverHTTP(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	f.Accounts = []sim.FixtureAccount{
		{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
	}
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()
	ctx := context.Background()

	connect := func(user, pass string) (bmc.Client, error) {
		return gofishclient.Connect(ctx, gofishclient.Config{Endpoint: srv.URL, Username: user, Password: pass})
	}

	factory, err := connect("admin", "factory")
	require.NoError(t, err)
	defer factory.Close()
	require.NoError(t, factory.EnsureAccount(ctx, "pk-metal", "InitialPass0"))

	managed, err := connect("pk-metal", "InitialPass0")
	require.NoError(t, err)
	_, err = managed.Manager(ctx)
	require.NoError(t, err, "the managed account works")

	require.NoError(t, factory.EnsureAccount(ctx, "pk-metal", "RotatedPass1"))
	_, err = managed.Manager(ctx)
	require.Error(t, err, "the old credential dies with the rotation")
	managed.Close()

	managed, err = connect("pk-metal", "RotatedPass1")
	require.NoError(t, err)
	defer managed.Close()
	_, err = managed.Manager(ctx)
	require.NoError(t, err, "the new credential works")

	bad, err := connect("pk-metal", "InitialPass0")
	require.NoError(t, err, "connecting alone proves nothing — the service root is public")
	defer bad.Close()
	_, err = bad.Manager(ctx)
	require.Error(t, err, "the first authenticated read is the oracle")
}

// TestReadOnlyRefusesMutation is the read-only rail: a read-only client refuses every write at
// this layer, regardless of what the caller asks.
func TestReadOnlyRefusesMutation(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()

	c, err := gofishclient.Connect(context.Background(),
		gofishclient.Config{Endpoint: srv.URL, ReadOnly: true})
	require.NoError(t, err)
	defer c.Close()
	ctx := context.Background()

	_, err = c.Systems(ctx)
	require.NoError(t, err, "read path must work")

	assert.Error(t, c.SetPower(ctx, "1", bmc.ActionForceOff), "SetPower on read-only")
	assert.Error(t, c.InsertMedia(ctx, "VirtualMedia1", "https://x/i.iso", nil), "InsertMedia on read-only")
	assert.Error(t, c.SetBootOverride(ctx, "1", "UsbCd", true), "SetBootOverride on read-only")
	assert.Error(t, c.UpdateFirmware(ctx, []byte("SIMFW 1")), "UpdateFirmware on read-only")
	assert.Error(t, c.ResetManager(ctx, bmc.ActionGracefulRestart), "ResetManager on read-only")
	assert.Error(t, c.SetBiosAttributes(ctx, "1", map[string]any{"QuietBoot": false}), "SetBiosAttributes on read-only")
}

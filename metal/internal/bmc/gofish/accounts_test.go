package gofish_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func connectAs(t *testing.T, url, user, password string) error {
	t.Helper()
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: url, Username: user, Password: password,
	})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Systems(context.Background())
	return err
}

// A fixed-slot collection whose first vacant slot is reserved and refuses a
// PATCH (iDRAC's slot 1): the account goes into the next vacant slot.
func TestEnsureAccountSkipsAReservedSlot(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.Accounts[0].ID = "2"
	f.ReservedAccountSlots = []string{"1"}
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()

	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
	})
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "managed-pass-1"))
	require.NoError(t, connectAs(t, srv.URL, "pk-metal", "managed-pass-1"))
}

// An account that failed logins locked stays locked after a new password on
// some BMCs. EnsureAccount clears the lock with the reset.
func TestEnsureAccountUnlocks(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	f.LockoutThreshold = 3
	st := sim.NewState(f)
	srv := httptest.NewServer(sim.NewServer(st))
	defer srv.Close()
	admin := f.Accounts[0]

	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: srv.URL, Username: admin.Username, Password: admin.Password,
	})
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "old-pass-1"))
	for range 3 {
		require.Error(t, connectAs(t, srv.URL, "pk-metal", "wrong-pass"))
	}
	require.True(t, st.AccountLocked("pk-metal"))
	require.Error(t, connectAs(t, srv.URL, "pk-metal", "old-pass-1"), "a locked account refuses the right password too")

	require.NoError(t, c.EnsureAccount(context.Background(), "pk-metal", "new-pass-1"))
	assert.False(t, st.AccountLocked("pk-metal"))
	require.NoError(t, connectAs(t, srv.URL, "pk-metal", "new-pass-1"))
}

// DisableAccount turns the account off over the wire, on both collection
// shapes: the account logs in no more, and the account that disabled it
// keeps working.
func TestDisableAccountOverHTTP(t *testing.T) {
	for name, slots := range map[string]int{"fixed-slots": 8, "growable": 0} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture("generic")
			require.NoError(t, err)
			f.AccountSlots = slots
			st := sim.NewState(f)
			srv := httptest.NewServer(sim.NewServer(st))
			defer srv.Close()
			admin := f.Accounts[0]

			c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
				Endpoint: srv.URL, Username: admin.Username, Password: admin.Password,
			})
			require.NoError(t, err)
			defer c.Close()
			require.NoError(t, c.EnsureAccount(context.Background(), "pk-old", "old-pass-1"))
			require.NoError(t, connectAs(t, srv.URL, "pk-old", "old-pass-1"))

			require.NoError(t, c.DisableAccount(context.Background(), "pk-old"))
			assert.False(t, st.AccountEnabled("pk-old"))
			require.Error(t, connectAs(t, srv.URL, "pk-old", "old-pass-1"), "a disabled account logs in no more")
			require.NoError(t, connectAs(t, srv.URL, admin.Username, admin.Password))
		})
	}
}

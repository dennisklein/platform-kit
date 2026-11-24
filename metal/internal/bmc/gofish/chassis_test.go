package gofish_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// twoChassisSim serves the generic sim with a second chassis, a storage
// enclosure with a serial of its own, listed before the board's chassis.
// With broken set, the enclosure answers 500 instead.
func twoChassisSim(t *testing.T, broken bool) (*httptest.Server, *sim.Fixture) {
	t.Helper()
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	next := sim.NewServer(sim.NewState(f))
	write := func(w http.ResponseWriter, doc any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}
	mux := http.NewServeMux()
	mux.Handle("/", next)
	mux.HandleFunc("GET /redfish/v1/Chassis", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Chassis",
			"Members": []map[string]string{
				{"@odata.id": "/redfish/v1/Chassis/Enclosure"},
				{"@odata.id": "/redfish/v1/Chassis/1"},
			},
			"Members@odata.count": 2,
		})
	})
	mux.HandleFunc("GET /redfish/v1/Chassis/Enclosure", func(w http.ResponseWriter, _ *http.Request) {
		if broken {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		write(w, map[string]any{
			"@odata.id": "/redfish/v1/Chassis/Enclosure", "@odata.type": "#Chassis.v1_26_0.Chassis",
			"Id": "Enclosure", "Name": "Storage Enclosure", "ChassisType": "Enclosure",
			"Model": "Backplane", "SerialNumber": "ENCLOSURE-SERIAL",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

// On a BMC with two chassis, the chassis the system links is the one read,
// on every pass: its board serial is an identity anchor and must not flap.
func TestChassisIsTheOneTheSystemLinks(t *testing.T) {
	srv, f := twoChassisSim(t, false)
	for range 20 {
		c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
			Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
		})
		require.NoError(t, err)
		ch, err := c.Chassis(context.Background())
		require.NoError(t, err)
		require.NoError(t, c.Close())
		require.Equal(t, f.Chassis.BoardSerial, ch.BoardSerial)
	}
}

// A chassis member that fails to read does not fail the read of the one
// that does.
func TestBrokenChassisMemberIsSkipped(t *testing.T) {
	srv, f := twoChassisSim(t, true)
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{
		Endpoint: srv.URL, Username: f.Accounts[0].Username, Password: f.Accounts[0].Password,
	})
	require.NoError(t, err)
	defer c.Close()
	ch, err := c.Chassis(context.Background())
	require.NoError(t, err)
	assert.Equal(t, f.Chassis.BoardSerial, ch.BoardSerial)
	_, err = c.Systems(context.Background())
	require.NoError(t, err, "the chassis drive sweep skips the broken member too")
}

// Drives without a serial number are told apart by their resource: two
// such drives are two drives, not one.
func TestDrivesWithoutSerialAreKept(t *testing.T) {
	for _, layout := range []string{"storage", "chassis"} {
		t.Run(layout, func(t *testing.T) {
			f, err := sim.LoadFixture("generic")
			require.NoError(t, err)
			f.Layout.Drives = layout
			require.NotEmpty(t, f.Systems[0].Drives)
			d := f.Systems[0].Drives[0]
			d.Serial = ""
			f.Systems[0].Drives = append(f.Systems[0].Drives[:0], d, d, d)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()
			systems, err := c.Systems(context.Background())
			require.NoError(t, err)
			assert.Len(t, systems[0].Drives, 3)
		})
	}
}

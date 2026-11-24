package gofish_test

import (
	"context"
	"crypto/x509"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// TestHardware runs the read-only conformance checks against a real BMC.
// It is skipped unless PK_METAL_HW_URL names one:
//
//	PK_METAL_HW_URL        the BMC, for example https://10.0.0.5
//	PK_METAL_HW_USER       the user (default admin)
//	PK_METAL_BMC_PASSWORD  the password
//	PK_METAL_HW_CA         a PEM file with the CA of the BMC's certificate;
//	                       without it the certificate is not checked
//
// The client is opened read-only: it refuses every write before it is sent.
func TestHardware(t *testing.T) {
	endpoint := os.Getenv("PK_METAL_HW_URL")
	if endpoint == "" {
		t.Skip("PK_METAL_HW_URL is not set")
	}
	cfg := gofishclient.Config{
		Endpoint: endpoint,
		Username: os.Getenv("PK_METAL_HW_USER"),
		Password: os.Getenv("PK_METAL_BMC_PASSWORD"),
		ReadOnly: true,
		Insecure: true,
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if caFile := os.Getenv("PK_METAL_HW_CA"); caFile != "" {
		pem, err := os.ReadFile(caFile)
		require.NoError(t, err)
		cfg.RootCAs = x509.NewCertPool()
		require.True(t, cfg.RootCAs.AppendCertsFromPEM(pem), "%s holds no PEM certificate", caFile)
		cfg.Insecure = false
	}
	c, err := gofishclient.Connect(context.Background(), cfg)
	require.NoError(t, err)
	defer c.Close()
	bmctest.RunReadOnly(t, c)
}

// The read-only checks pass against every fixture, so that a failure on
// hardware is the hardware's and not the checks'.
func TestReadOnlyChecksOnTheSim(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			cfg := gofishclient.Config{Endpoint: srv.URL, ReadOnly: true}
			if len(f.Accounts) > 0 {
				cfg.Username, cfg.Password = f.Accounts[0].Username, f.Accounts[0].Password
			}
			c, err := gofishclient.Connect(context.Background(), cfg)
			require.NoError(t, err)
			defer c.Close()
			bmctest.RunReadOnly(t, c)
		})
	}
}

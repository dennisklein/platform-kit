package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"flag"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

func parseBMCFlags(t *testing.T, args ...string) bmcFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bf := connectFlags(fs)
	require.NoError(t, fs.Parse(args))
	return bf
}

// --bmc-ca pins the BMC's certificate to the given CA: the session reaches a
// BMC whose certificate chains to it, and the same BMC without the pin is
// refused.
func TestBMCCAPinsTheCertificate(t *testing.T) {
	f, err := sim.LoadFixture("generic")
	require.NoError(t, err)
	srv := httptest.NewTLSServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "bmc-ca.pem")
	require.NoError(t, os.WriteFile(caFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	acct := f.Accounts[0]

	var warn bytes.Buffer
	cfg, err := parseBMCFlags(t, "--user", acct.Username, "--bmc-ca", caFile).
		config(srv.URL, acct.Password, true, &warn)
	require.NoError(t, err)
	assert.Empty(t, warn.String(), "no warning with a pinned CA")
	c, err := gofishclient.Connect(context.Background(), cfg)
	require.NoError(t, err)
	_, err = c.Systems(context.Background())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	cfg, err = parseBMCFlags(t, "--user", acct.Username).config(srv.URL, acct.Password, true, &warn)
	require.NoError(t, err)
	_, err = gofishclient.Connect(context.Background(), cfg)
	require.Error(t, err, "without --bmc-ca the self-signed BMC is refused")
}

func TestBMCFlagsRefuseBadInput(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate\n"), 0o600))

	_, err := parseBMCFlags(t, "--bmc-ca", empty).config("https://bmc", "", true, io.Discard)
	require.ErrorContains(t, err, "holds no PEM certificate")
	_, err = parseBMCFlags(t, "--bmc-ca", empty, "--insecure").config("https://bmc", "", true, io.Discard)
	require.ErrorContains(t, err, "not both")
}

func TestInsecureWarns(t *testing.T) {
	var warn bytes.Buffer
	cfg, err := parseBMCFlags(t, "--insecure").config("https://bmc", "", true, &warn)
	require.NoError(t, err)
	assert.True(t, cfg.Insecure)
	assert.Contains(t, warn.String(), "--bmc-ca")
}

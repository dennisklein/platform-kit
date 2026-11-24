package gofish_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/testpki"
)

// The iBMC takes a while to make a key, and HttpsCert serves the previous
// request until then. A second GenerateCSR must wait for the new request:
// a certificate signed for the previous one does not match the key the
// manager holds afterwards, and its import fails.
func TestSlowCSRIsNotTheStaleOne(t *testing.T) {
	f, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(t, err)
	require.True(t, f.Identity.CSRNative)
	f.Identity.CSRDelay = 300 * time.Millisecond
	srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
	defer srv.Close()
	c, err := gofishclient.Connect(context.Background(), gofishclient.Config{Endpoint: srv.URL})
	require.NoError(t, err)
	defer c.Close()

	subject := bmc.CSRSubject{CommonName: "bmc.example.internal", Country: "XX"}
	ctx := context.Background()
	first, err := c.GenerateCSR(ctx, subject)
	require.NoError(t, err)
	second, err := c.GenerateCSR(ctx, subject)
	require.NoError(t, err)
	assert.NotEqual(t, string(first), string(second), "the second request is the new one")

	block, _ := pem.Decode(second)
	require.NotNil(t, block)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	chain, err := testpki.NewCA(t, "csr-test-ca").Sign(csr, time.Hour)
	require.NoError(t, err)
	require.NoError(t, c.ImportServerCertificate(ctx, chain))
}

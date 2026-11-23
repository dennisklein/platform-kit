package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func selfSignedPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestFirstCertificate(t *testing.T) {
	leafPEM := selfSignedPEM(t, "bmc-01.example.test")
	caPEM := selfSignedPEM(t, "Example Test CA")

	t.Run("leaf of a chain", func(t *testing.T) {
		leaf, got, err := firstCertificate(append(append([]byte{}, leafPEM...), caPEM...))
		require.NoError(t, err)
		assert.Equal(t, "bmc-01.example.test", leaf.Subject.CommonName)
		assert.Equal(t, leafPEM, got, "the leaf re-encodes to the bytes it was issued as")
	})

	t.Run("broken intermediate refused", func(t *testing.T) {
		broken := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})
		_, _, err := firstCertificate(append(append([]byte{}, leafPEM...), broken...))
		assert.Error(t, err, "the whole chain is pushed to the manager, so every certificate must parse")
	})

	t.Run("no certificate", func(t *testing.T) {
		_, _, err := firstCertificate([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"))
		assert.Error(t, err)
	})
}

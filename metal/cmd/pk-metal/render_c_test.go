package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

// renderInputs writes the sample manifests into a fresh directory and returns
// it with the path of the test secrets bundle.
func renderInputs(t *testing.T) (dir, secrets string) {
	t.Helper()
	dir = t.TempDir()
	sample, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "talos.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "talos.yaml"), sample, 0o600))
	return dir, filepath.Join("..", "..", "internal", "talos", "render", "testdata", "secrets.yaml")
}

// The admin credentials live as long as --credentials-ttl says, 24h by
// default, not a year.
func TestRenderCredentialsTTL(t *testing.T) {
	in, secrets := renderInputs(t)
	notAfter := func(out string) time.Time {
		kc, err := clientcmd.LoadFromFile(filepath.Join(out, "kubeconfig"))
		require.NoError(t, err)
		for _, auth := range kc.AuthInfos {
			block, _ := pem.Decode(auth.ClientCertificateData)
			require.NotNil(t, block)
			cert, err := x509.ParseCertificate(block.Bytes)
			require.NoError(t, err)
			return cert.NotAfter
		}
		t.Fatal("the kubeconfig holds no client certificate")
		return time.Time{}
	}

	out := filepath.Join(t.TempDir(), "default")
	renderCmd([]string{"--dir", in, "--secrets", secrets, "--out", out})
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), notAfter(out), 5*time.Minute)

	out = filepath.Join(t.TempDir(), "short")
	renderCmd([]string{"--dir", in, "--secrets", secrets, "--out", out, "--credentials-ttl", "2h"})
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), notAfter(out), 5*time.Minute)
}

func TestRenderNoCredentials(t *testing.T) {
	in, secrets := renderInputs(t)
	out := filepath.Join(t.TempDir(), "out")
	renderCmd([]string{"--dir", in, "--secrets", secrets, "--out", out, "--no-credentials"})

	nodes, err := filepath.Glob(filepath.Join(out, "*.yaml"))
	require.NoError(t, err)
	assert.NotEmpty(t, nodes, "the node configs are written")
	assert.NoFileExists(t, filepath.Join(out, "kubeconfig"))
	assert.NoFileExists(t, filepath.Join(out, "talosconfig"))
}

func TestRenderRefusesLongCredentials(t *testing.T) {
	for _, ttl := range []string{"721h", "0s"} {
		code, stderr := runCLI(t, "render", "--dir=d", "--secrets=s", "--credentials-ttl="+ttl)
		assert.Equal(t, 1, code, stderr)
		assert.Contains(t, stderr, "--credentials-ttl must be more than 0 and at most 720h")
	}
}

// A directory or file that existed with wider permissions is narrowed:
// MkdirAll and WriteFile alone keep an existing mode.
func TestWriteSecretFilesNarrowsExistingModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	old := filepath.Join(dir, "kubeconfig")
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o644))
	require.NoError(t, os.Chmod(old, 0o644))

	require.NoError(t, writeSecretFiles(dir, map[string][]byte{"kubeconfig": []byte("new"), "cp-1.yaml": []byte("cfg")}))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	for _, name := range []string{"kubeconfig", "cp-1.yaml"} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
	}
	data, err := os.ReadFile(old)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

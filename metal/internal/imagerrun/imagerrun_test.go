package imagerrun

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The config a test embeds: a synthetic token stands for the secrets.
const config = `version: v1alpha1
machine:
  type: worker
  token: synthetic0token0abcdef
  ca:
    crt: LS0tLS1CRUdJTiBzeW50aGV0aWMgY2VydA==
cluster:
  secret: synthetic0cluster0secret
`

// TestMain lets the test binary play the imager: with PK_FAKE_IMAGER set it
// prints what the imager prints (progress, then the profile with the config
// in it) and exits with the code asked for.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PK_FAKE_IMAGER"); mode != "" {
		cfg, _ := os.ReadFile(os.Getenv("PK_FAKE_CONFIG"))
		fmt.Fprintln(os.Stderr, "assembling the finalized profile...")
		fmt.Fprintln(os.Stderr, "profile ready:")
		fmt.Fprintln(os.Stderr, "arch: amd64\ncustomization:\n  embeddedMachineConfiguration: |")
		for _, l := range strings.Split(string(cfg), "\n") {
			fmt.Fprintln(os.Stderr, "    "+l)
		}
		// The quoted form a YAML encoder falls back to.
		fmt.Fprintf(os.Stdout, "embedded: %q\n", string(cfg))
		fmt.Fprintln(os.Stderr, "building ISO...")
		if mode == "fail" {
			fmt.Fprintln(os.Stderr, "error: xorriso: no space left on device")
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeImager(t *testing.T, mode string) (string, string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte(config), 0o400))
	t.Setenv("PK_FAKE_IMAGER", mode)
	t.Setenv("PK_FAKE_CONFIG", cfg)
	return os.Args[0], cfg
}

func assertNoSecret(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{"synthetic0token0abcdef", "LS0tLS1CRUdJTiBzeW50aGV0aWMgY2VydA==", "synthetic0cluster0secret"} {
		assert.NotContains(t, out, s)
	}
}

func TestRunWithholdsTheOutputOfASuccess(t *testing.T) {
	bin, cfg := fakeImager(t, "ok")
	var out bytes.Buffer
	code := Run(context.Background(), []string{bin}, cfg, &out)
	assert.Equal(t, 0, code)
	assertNoSecret(t, out.String())
	assert.Contains(t, out.String(), "succeeded")
	assert.NotContains(t, out.String(), "building ISO", "a success prints a summary only")
}

func TestRunPrintsARedactedTailOfAFailure(t *testing.T) {
	bin, cfg := fakeImager(t, "fail")
	var out bytes.Buffer
	code := Run(context.Background(), []string{bin}, cfg, &out)
	assert.Equal(t, 3, code, "the imager's exit code is the Job's")
	assertNoSecret(t, out.String())
	assert.Contains(t, out.String(), "no space left on device", "what went wrong stays readable")
	assert.Contains(t, out.String(), "building ISO")
	assert.Contains(t, out.String(), "line withheld")
}

func TestRunRefusesWithoutTheConfig(t *testing.T) {
	var out bytes.Buffer
	code := Run(context.Background(), []string{"/bin/true"}, filepath.Join(t.TempDir(), "missing"), &out)
	assert.Equal(t, 2, code)
}

func TestTailKeepsTheLastLines(t *testing.T) {
	tl := newTail(3)
	for i := range 10 {
		_, _ = fmt.Fprintf(tl, "line %d\n", i)
	}
	_, _ = tl.Write([]byte("partial"))
	assert.Equal(t, []string{"line 8", "line 9", "partial"}, tl.lines())
}

func TestStageCopiesTheRunningBinary(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "manager")
	require.NoError(t, Stage(dest))
	st, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o555), st.Mode().Perm())
	self, err := os.Executable()
	require.NoError(t, err)
	want, err := os.Stat(self)
	require.NoError(t, err)
	assert.Equal(t, want.Size(), st.Size())
}

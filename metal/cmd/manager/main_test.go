package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

func TestParseFlagsDefaults(t *testing.T) {
	c, err := parseFlags([]string{"--ledger=/etc/pk-metal/ledger.yaml"}, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "/etc/pk-metal/ledger.yaml", c.ledgerPath)
	// The deployment in metal/config relies on these: the namespace its Role
	// covers, the timeouts the operation schemas allow, metrics and probes off.
	assert.Equal(t, "pk-metal", c.credsNS)
	assert.Equal(t, 5*time.Minute, c.shutdown)
	assert.Equal(t, 30*time.Minute, c.checkIn)
	assert.Equal(t, 5*time.Minute, c.poll)
	assert.Equal(t, render.PublicFactory, c.imageFactory)
	assert.Equal(t, ":8443", c.mediaListen)
	assert.Empty(t, c.mediaURL, "no media service unless a URL is given")
	assert.Equal(t, "0", c.metricsAddr)
	assert.Equal(t, "0", c.probeAddr)
	assert.False(t, c.leaderElect)
	assert.Nil(t, c.endpointTLS(), "BMCs are checked against the system CAs by default")
}

func TestParseFlagsRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no ledger", nil, "--ledger is required"},
		{"shutdown too short", []string{"--ledger=l", "--release-shutdown-timeout=500ms"}, "between 1s and 1h"},
		{"shutdown too long", []string{"--ledger=l", "--release-shutdown-timeout=2h"}, "between 1s and 1h"},
		{"no poll interval", []string{"--ledger=l", "--poll-interval=0s"}, "--poll-interval must be positive"},
		{"no check-in timeout", []string{"--ledger=l", "--checkin-timeout=0s"}, "--checkin-timeout must be positive"},
		{"stray argument", []string{"--ledger=l", "ledger.yaml"}, "every setting is a flag"},
		{"unknown flag", []string{"--ledger=l", "--bmc-insecur"}, "flag provided but not defined"},
		{"not a duration", []string{"--ledger=l", "--poll-interval=5"}, "invalid value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := parseFlags(tc.args, &out)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestParseFlagsVersionAndHelp(t *testing.T) {
	c, err := parseFlags([]string{"--version"}, &bytes.Buffer{})
	require.NoError(t, err, "--version needs no ledger")
	assert.True(t, c.showVersion)

	var out bytes.Buffer
	_, err = parseFlags([]string{"-h"}, &out)
	assert.True(t, errors.Is(err, flag.ErrHelp))
	assert.Contains(t, out.String(), "-release-shutdown-timeout", "the usage lists the flags")
}

func TestEndpointTLS(t *testing.T) {
	c, err := parseFlags([]string{"--ledger=l", "--bmc-insecure"}, &bytes.Buffer{})
	require.NoError(t, err)
	tls := c.endpointTLS()
	require.NotNil(t, tls)
	assert.True(t, tls.InsecureSkipVerify)

	c, err = parseFlags([]string{"--ledger=l", "--bmc-insecure", "--bmc-ca-secret=bmc-ca"}, &bytes.Buffer{})
	require.NoError(t, err)
	tls = c.endpointTLS()
	require.NotNil(t, tls)
	assert.False(t, tls.InsecureSkipVerify, "a pinned CA wins over --bmc-insecure")
	assert.Equal(t, &corev1.LocalObjectReference{Name: "bmc-ca"}, tls.CASecretRef)
}

// The flags the committed deployment and the lab overlay pass must parse:
// a renamed or dropped flag otherwise shows up only as a crash-looping pod.
func TestCommittedArgsParse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "manager", "deployment.yaml"))
	require.NoError(t, err)
	var deploy appsv1.Deployment
	require.NoError(t, yaml.Unmarshal(raw, &deploy))
	require.Len(t, deploy.Spec.Template.Spec.Containers, 1)
	args := deploy.Spec.Template.Spec.Containers[0].Args

	c, err := parseFlags(args, &bytes.Buffer{})
	require.NoError(t, err, "config/manager/deployment.yaml: %v", args)
	assert.True(t, c.leaderElect)
	assert.Equal(t, "pk-metal", c.credsNS)

	raw, err = os.ReadFile(filepath.Join("..", "..", "config", "lab", "manager_args.yaml"))
	require.NoError(t, err)
	var ops []struct {
		Value string `json:"value"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &ops))
	for _, op := range ops {
		args = append(args, op.Value)
	}
	c, err = parseFlags(args, &bytes.Buffer{})
	require.NoError(t, err, "with config/lab/manager_args.yaml: %v", args)
	assert.NotEmpty(t, c.mediaURL)
	assert.True(t, c.bmcInsecure)
}

func TestCacheOptionsConfineSecretsAndConfigMaps(t *testing.T) {
	opts := cacheOptions("pk-metal")
	require.Len(t, opts.ByObject, 2)
	for obj, by := range opts.ByObject {
		switch obj.(type) {
		case *corev1.Secret, *corev1.ConfigMap:
		default:
			t.Fatalf("unexpected confined kind %T", obj)
		}
		assert.Len(t, by.Namespaces, 1, "%T", obj)
		assert.Contains(t, by.Namespaces, "pk-metal", "%T: the namespaced Role covers only this one", obj)
	}
}

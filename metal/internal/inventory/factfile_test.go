package inventory_test

import (
	"bytes"
	"context"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/inventory"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGolden byte-compares the rendered fact file per fixture against testdata goldens
// (timestamps pinned) — through BOTH clients: the in-process sim state AND the gofish
// client over the sim's HTTP tree. The latter is what caught gofish's concurrent
// collection walks reordering lists; identical bytes from both paths is the contract.
// The cross-check against the REAL control/inventory files happens on real hardware.
func TestGolden(t *testing.T) {
	ts := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for name, node := range map[string]string{
		"huawei-2288h-v5": "ref-infra",
		"supermicro-x12":  "ref-x12",
	} {
		f, err := sim.LoadFixture(name)
		require.NoError(t, err)

		clients := map[string]func(t *testing.T) bmc.Client{
			"state": func(_ *testing.T) bmc.Client { return sim.NewState(f) },
			"gofish": func(t *testing.T) bmc.Client {
				srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
				t.Cleanup(srv.Close)
				c, err := gofishclient.Connect(context.Background(), gofishclient.Config{Endpoint: srv.URL})
				require.NoError(t, err)
				t.Cleanup(func() { _ = c.Close() })
				return c
			},
		}
		for clientName, mk := range clients {
			t.Run(name+"/"+clientName, func(t *testing.T) {
				facts, err := inventory.Gather(context.Background(), mk(t))
				require.NoError(t, err)

				var buf bytes.Buffer
				inventory.Write(&buf, node, "sim", ts, facts)

				// Whatever the format, it must stay parseable YAML.
				var parsed map[string]any
				require.NoError(t, yaml.Unmarshal(buf.Bytes(), &parsed), "output must be valid YAML")

				golden := filepath.Join("testdata", name+".golden.yaml")
				if *update && clientName == "state" {
					require.NoError(t, os.WriteFile(golden, buf.Bytes(), 0o644))
					return
				}
				want, err := os.ReadFile(golden)
				require.NoError(t, err, "run: go test ./internal/inventory -update")
				assert.Equal(t, string(want), buf.String(), "fact file drifted from %s (%s client)", golden, clientName)
			})
		}
	}
}

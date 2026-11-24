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
// The cross-check against fact files from real BMCs happens on real hardware.
func TestGolden(t *testing.T) {
	ts := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	for name, node := range map[string]string{
		"huawei-2288h-v5": "node-infra",
		"supermicro-x12":  "node-x12",
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

// TestRoundTrip parses the written file back with a YAML library and checks
// every string arrives as written. The values are what a BMC can serve:
// quotes, backslashes, '#', control characters, non-printable Unicode, and
// bytes that are not UTF-8 at all. Go's %q and YAML's double-quoted escapes
// agree on most of these but not on invalid UTF-8: Go writes the byte as
// \xff, YAML reads \xff as the code point U+00FF.
func TestRoundTrip(t *testing.T) {
	odd := "a\"b\\c # d\te\x01f g h"
	bad := "SN\xff\xfe01" // not UTF-8
	facts := inventory.Facts{
		Manager: bmc.Manager{FirmwareVersion: odd, Hostname: bad, MAC: "02:00:00:00:00:01"},
		Chassis: bmc.Chassis{Model: "board " + odd, Adapters: []bmc.Adapter{{Model: bad, PortMACs: []string{odd}}}},
		Media:   []bmc.VirtualMedia{{ID: bad, MediaTypes: []string{odd, bad}, OemActions: []string{"#X"}}},
		Systems: []bmc.System{{
			Manufacturer: odd, Model: "model", SerialNumber: bad, UUID: "u", PowerState: "On",
			CPUModel: odd, NICs: []bmc.NIC{{ID: bad, MAC: odd}},
			Drives: []bmc.Drive{{Model: odd, Serial: bad, MediaType: "SSD", Protocol: odd}},
		}},
	}
	var buf bytes.Buffer
	inventory.Write(&buf, "node-01", "192.0.2.10", time.Unix(0, 0), facts)

	var got struct {
		BMC struct {
			Address, Firmware, Hostname string
			VirtualMedia                []struct {
				ID         string   `yaml:"id"`
				MediaTypes []string `yaml:"media_types"`
			} `yaml:"virtualmedia"`
		} `yaml:"bmc"`
		System struct {
			Manufacturer, Board, Serial string
			CPU                         struct{ Model string }
		}
		NICs     []struct{ ID, MAC string }
		Adapters []struct {
			Model    string
			PortMACs []string `yaml:"port_macs"`
		}
		Drives []struct{ Model, Serial, Protocol string }
	}
	require.NoError(t, yaml.Unmarshal(buf.Bytes(), &got), "output must be valid YAML:\n%s", buf.String())

	// YAML text is Unicode, so bytes that are not UTF-8 cannot survive as
	// such; they must become U+FFFD rather than some other, plausible
	// character.
	badRead := "SN�01"
	assert.Equal(t, "192.0.2.10", got.BMC.Address)
	assert.Equal(t, odd, got.BMC.Firmware)
	assert.Equal(t, badRead, got.BMC.Hostname)
	require.Len(t, got.BMC.VirtualMedia, 1)
	assert.Equal(t, badRead, got.BMC.VirtualMedia[0].ID)
	assert.Equal(t, []string{odd, badRead}, got.BMC.VirtualMedia[0].MediaTypes)
	assert.Equal(t, odd, got.System.Manufacturer)
	assert.Equal(t, "board "+odd, got.System.Board)
	assert.Equal(t, badRead, got.System.Serial)
	assert.Equal(t, odd, got.System.CPU.Model)
	require.Len(t, got.NICs, 1)
	assert.Equal(t, badRead, got.NICs[0].ID)
	assert.Equal(t, odd, got.NICs[0].MAC)
	require.Len(t, got.Adapters, 1)
	assert.Equal(t, badRead, got.Adapters[0].Model)
	assert.Equal(t, []string{odd}, got.Adapters[0].PortMACs)
	require.Len(t, got.Drives, 1)
	assert.Equal(t, odd, got.Drives[0].Model)
	assert.Equal(t, badRead, got.Drives[0].Serial)
	assert.Equal(t, odd, got.Drives[0].Protocol)
}

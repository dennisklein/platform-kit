package inventory_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/inventory"
)

// FuzzFactFile: BMC strings are untrusted, and a fact file that parses to
// another value than the BMC sent invents a fact. Every string written must
// read back as itself (bytes that are not UTF-8 as U+FFFD).
func FuzzFactFile(f *testing.F) {
	f.Add("node-infra", "Huawei", "2288H V5", "SIMHW2288V5000391", "0c:c4:7a:12:34:56", "SAMSUNG MZ7LH480")
	f.Add("n1", "", "", "", "", "")
	f.Add("n1", "a # b", "x: y", "'q'", "\"dq\"", "line\nbreak")
	f.Add("n1", "\xff\xfe", " ", "\x00", "\t", "- item")
	f.Add("n1", "yes", "null", "~", "0x10", "1e3")
	f.Fuzz(func(t *testing.T, node, manufacturer, model, serial, mac, driveModel string) {
		facts := inventory.Facts{
			Manager: bmc.Manager{MAC: mac},
			Systems: []bmc.System{{
				Manufacturer: manufacturer,
				Model:        model,
				SerialNumber: serial,
				Drives:       []bmc.Drive{{Model: driveModel, Serial: "S1"}},
			}},
		}
		var buf bytes.Buffer
		inventory.Write(&buf, node, "https://10.0.0.21", time.Unix(0, 0), facts)

		var doc struct {
			Node string `yaml:"node"`
			BMC  struct {
				MAC string `yaml:"mac"`
			} `yaml:"bmc"`
			System struct {
				Manufacturer string `yaml:"manufacturer"`
				Model        string `yaml:"model"`
				Serial       string `yaml:"serial"`
			} `yaml:"system"`
			Drives []struct {
				Model string `yaml:"model"`
			} `yaml:"drives"`
		}
		require.NoError(t, yaml.Unmarshal(buf.Bytes(), &doc), "fact file:\n%s", buf.String())
		want := func(s string) string {
			if s == "" {
				return "?"
			}
			return strings.ToValidUTF8(s, "�")
		}
		assert.Equal(t, strings.ToValidUTF8(node, "�"), doc.Node, "node")
		assert.Equal(t, want(mac), doc.BMC.MAC, "mac")
		assert.Equal(t, want(manufacturer), doc.System.Manufacturer, "manufacturer")
		assert.Equal(t, want(model), doc.System.Model, "model")
		assert.Equal(t, want(serial), doc.System.Serial, "serial")
		require.Len(t, doc.Drives, 1)
		assert.Equal(t, strings.ToValidUTF8(driveModel, "�"), doc.Drives[0].Model, "drive model")
	})
}

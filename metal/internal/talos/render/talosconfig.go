package render

import (
	"time"

	"github.com/budimanjojo/talhelper/v3/pkg/config"
	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
)

// Talosconfig renders client credentials in memory: a TTL-bounded admin cert
// minted from the secrets bundle (talhelper's client-config generation — the
// CLI writes this to disk once per cluster; the operator's check-in mints a
// short-lived one per dial and never persists it). The nodes section is
// omitted — callers name their target explicitly.
func Talosconfig(c *config.TalhelperConfig, input *generate.Input, ttl time.Duration) ([]byte, error) {
	return thtalos.GenerateClientConfigBytes(c, input, true, ttl)
}

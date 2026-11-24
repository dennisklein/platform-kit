package gofish

import (
	"context"
	"fmt"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// ntpSurface is the discovered time-source dialect: the OEM NtpService link
// (iBMC — its NetworkProtocol serves no NTP key at all) or the standard
// NetworkProtocol NTP block. Probed per call, one to two GETs.
type ntpSurface struct {
	oemURI      string
	standardURI string
}

func (c *client) discoverNTP() (ntpSurface, error) {
	var doc struct {
		NetworkProtocol ref `json:"NetworkProtocol"`
		Oem             map[string]struct {
			NtpService ref `json:"NtpService"`
		} `json:"Oem"`
	}
	if err := c.managerDoc(&doc); err != nil {
		return ntpSurface{}, err
	}
	for _, oem := range doc.Oem {
		if oem.NtpService.ODataID != "" {
			return ntpSurface{oemURI: oem.NtpService.ODataID}, nil
		}
	}
	if doc.NetworkProtocol.ODataID != "" {
		return ntpSurface{standardURI: doc.NetworkProtocol.ODataID}, nil
	}
	return ntpSurface{}, fmt.Errorf("bmc: endpoint exposes no NTP surface")
}

func (c *client) NTP(ctx context.Context) (bmc.NTPConfig, error) {
	surf, err := c.discoverNTP()
	if err != nil {
		return bmc.NTPConfig{}, err
	}
	if surf.oemURI != "" {
		var doc struct {
			ServiceEnabled     bool
			PreferredNtpServer string
			AlternateNtpServer string
			ExtraNtpServer     string
		}
		if _, err := c.getJSON(surf.oemURI, &doc); err != nil {
			return bmc.NTPConfig{}, fmt.Errorf("bmc: ntp service: %w", err)
		}
		out := bmc.NTPConfig{Enabled: doc.ServiceEnabled}
		for _, s := range []string{doc.PreferredNtpServer, doc.AlternateNtpServer, doc.ExtraNtpServer} {
			if s != "" {
				out.Servers = append(out.Servers, s)
			}
		}
		return out, nil
	}
	var doc struct {
		NTP *struct {
			ProtocolEnabled bool
			NTPServers      []string
		}
	}
	if _, err := c.getJSON(surf.standardURI, &doc); err != nil {
		return bmc.NTPConfig{}, fmt.Errorf("bmc: network protocol: %w", err)
	}
	if doc.NTP == nil {
		return bmc.NTPConfig{}, fmt.Errorf("bmc: endpoint exposes no NTP surface")
	}
	out := bmc.NTPConfig{Enabled: doc.NTP.ProtocolEnabled}
	for _, s := range doc.NTP.NTPServers {
		if s != "" {
			out.Servers = append(out.Servers, s)
		}
	}
	return out, nil
}

func (c *client) SetNTP(ctx context.Context, cfg bmc.NTPConfig) error {
	if c.readOnly {
		return errReadOnly
	}
	surf, err := c.discoverNTP()
	if err != nil {
		return err
	}
	if surf.oemURI != "" {
		if len(cfg.Servers) > 3 {
			return fmt.Errorf("bmc: at most 3 NTP servers on this endpoint")
		}
		slot := func(i int) string {
			if i < len(cfg.Servers) {
				return cfg.Servers[i]
			}
			return "" // clears the slot
		}
		if err := c.patch(surf.oemURI, map[string]any{
			"ServiceEnabled":     cfg.Enabled,
			"PreferredNtpServer": slot(0),
			"AlternateNtpServer": slot(1),
			"ExtraNtpServer":     slot(2),
		}); err != nil {
			return fmt.Errorf("bmc: ntp service: %w", err)
		}
		return nil
	}
	servers := cfg.Servers
	if servers == nil {
		servers = []string{} // an empty list clears; null would not be a list
	}
	if err := c.patch(surf.standardURI, map[string]any{
		"NTP": map[string]any{"ProtocolEnabled": cfg.Enabled, "NTPServers": servers},
	}); err != nil {
		return fmt.Errorf("bmc: network protocol: %w", err)
	}
	return nil
}

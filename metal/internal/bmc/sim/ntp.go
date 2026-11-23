package sim

import (
	"context"
	"fmt"
	"slices"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The NTP model, capture-true per vendor: the X12 serves the standard
// NetworkProtocol NTP block; the iBMC serves NO NTP key there and carries an
// OEM HwNtpService with three discrete server slots, linked under
// Manager Oem.Huawei. The seam view is one NTPConfig either way; the HTTP
// tier serves each fixture its own shape.

// ntpServed reports whether the fixture models any NTP surface.
func (f *Fixture) ntpServed() bool { return f.NTP.Standard || f.NTP.OemService }

func (s *State) NTP(context.Context) (bmc.NTPConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.NTPConfig{}, err
	}
	if !s.f.ntpServed() {
		return bmc.NTPConfig{}, fmt.Errorf("sim: fixture models no NTP surface")
	}
	return bmc.NTPConfig{Enabled: s.ntp.Enabled, Servers: slices.Clone(s.ntp.Servers)}, nil
}

func (s *State) SetNTP(_ context.Context, cfg bmc.NTPConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return err
	}
	if !s.f.ntpServed() {
		return fmt.Errorf("sim: fixture models no NTP surface")
	}
	servers := slices.DeleteFunc(slices.Clone(cfg.Servers), func(s string) bool { return s == "" })
	if s.f.NTP.OemService && len(servers) > 3 {
		// The iBMC has exactly three server slots.
		return fmt.Errorf("sim: at most 3 NTP servers")
	}
	s.ntp = bmc.NTPConfig{Enabled: cfg.Enabled, Servers: servers}
	return nil
}

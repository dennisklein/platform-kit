package sim

import (
	"context"
	"slices"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// The NTP model: the standard tree (and the X12) serves the
// NetworkProtocol NTP block; the iBMC serves NO NTP key there and carries an
// OEM HwNtpService with three discrete server slots, linked under
// Manager Oem.Huawei. The seam view is one NTPConfig either way; the HTTP
// tier serves each fixture its own shape.

// ntpServed reports whether the fixture models any NTP surface.
func (f *Fixture) ntpServed() bool { return f.NTP.Standard || f.NTP.OemService }

// ntpSlots is the number of NTP server slots; zero is no limit. The iBMC
// has exactly three.
func (f *Fixture) ntpSlots() int {
	if f.NTP.Slots == 0 && f.NTP.OemService {
		return 3
	}
	return f.NTP.Slots
}

func (s *State) NTP(context.Context) (bmc.NTPConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reachable(); err != nil {
		return bmc.NTPConfig{}, err
	}
	if !s.f.ntpServed() {
		return bmc.NTPConfig{}, refuse("fixture models no NTP surface")
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
		return refuse("fixture models no NTP surface")
	}
	servers := slices.DeleteFunc(slices.Clone(cfg.Servers), func(s string) bool { return s == "" })
	if slots := s.f.ntpSlots(); slots > 0 && len(servers) > slots {
		return refuse("at most %d NTP servers", slots)
	}
	s.ntp = bmc.NTPConfig{Enabled: cfg.Enabled, Servers: servers}
	return nil
}

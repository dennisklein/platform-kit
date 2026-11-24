package sim

import (
	"context"
	"net/url"
)

// Snapshot is the BMC's world as an observer outside Redfish sees it: power,
// boot override and virtual media, read in one go. It is for dashboards and
// diagnostics, not for the code under test: it ignores injected faults (an
// unreachable BMC still has a state) and carries no credentials.
type Snapshot struct {
	Systems []SystemSnapshot `json:"systems"`
	Media   []MediaSnapshot  `json:"media"`
}

// SystemSnapshot is one system's power and boot view.
type SystemSnapshot struct {
	ID    string `json:"id"`
	Power string `json:"power"`
	// BootProgress is what a fixture that serves BootProgress would report:
	// empty where the fixture does not serve it.
	BootProgress string               `json:"bootProgress"`
	BootOverride BootOverrideSnapshot `json:"bootOverride"`
}

// BootOverrideSnapshot is the Boot object as served.
type BootOverrideSnapshot struct {
	Target  string `json:"target"`
	Enabled string `json:"enabled"`
	Mode    string `json:"mode"`
}

// MediaSnapshot is one virtual media slot.
type MediaSnapshot struct {
	ID       string `json:"id"`
	Inserted bool   `json:"inserted"`
	// ImageURL is the mounted image without any user:password part: the vmm
	// dialect keeps the credentials in the URL, and a dashboard must not
	// show them.
	ImageURL string `json:"imageURL"`
}

// Snapshot reads the BMC's world. Like any Redfish read, it refreshes the
// power view from the machines behind the systems first.
func (s *State) Snapshot(ctx context.Context) Snapshot {
	s.syncMachines(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{Systems: []SystemSnapshot{}, Media: []MediaSnapshot{}}
	for i := range s.f.Systems {
		fs := &s.f.Systems[i]
		sys := SystemSnapshot{ID: fs.ID, Power: string(s.power[fs.ID]), BootProgress: s.bootProgress(fs)}
		sys.BootOverride.Enabled, sys.BootOverride.Target, sys.BootOverride.Mode = s.bootViewLocked(fs.ID)
		snap.Systems = append(snap.Systems, sys)
	}
	for _, vm := range s.f.VirtualMedia {
		m := MediaSnapshot{ID: vm.ID}
		if mm := s.media[vm.ID]; mm != nil {
			m.Inserted, m.ImageURL = true, WithoutUserinfo(mm.imageURL)
		}
		snap.Media = append(snap.Media, m)
	}
	return snap
}

// WithoutUserinfo drops the user:password part of a URL. A string that does
// not parse is not shown at all, since nothing tells where its secret ends.
func WithoutUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(not a URL)"
	}
	u.User = nil
	return u.String()
}

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/budimanjojo/talhelper/v3/pkg/config"
	thtalos "github.com/budimanjojo/talhelper/v3/pkg/talos"
	"github.com/siderolabs/image-factory/pkg/schematic"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

// PublicFactory is the image factory Sidero operates. An installation that
// cannot reach it runs its own — same API, so it is a matter of configuration.
const PublicFactory = "factory.talos.dev"

// Factory addresses a Talos image factory: it computes the URLs of the images
// that factory serves, and tells it about the schematics those URLs name.
//
// Both halves must address the same host, which is why they live on one type.
// A boot medium URL pointing at one factory while its schematic was registered
// with another is a 404 that only appears on the machine.
type Factory struct {
	// RegistryURL is the factory's host[:port]. Empty selects PublicFactory.
	RegistryURL string
	// HTTPClient talks to it. Nil selects one with a bounded timeout: a
	// reconciler must not be able to block on someone else's server forever.
	HTTPClient *http.Client

	// registered remembers the ids this process has already posted. Posting is
	// idempotent, but a reconciler that re-renders on every event should not
	// re-post on every event. Failures are not remembered, so they retry.
	registered sync.Map
}

// NewFactory addresses an image factory, or the public one when url is empty.
func NewFactory(url string) *Factory { return &Factory{RegistryURL: url} }

// Schematic maps the CRD's schematic onto the factory's own type — the shape
// whose content the id is a hash of. Every field the CRD carries must land
// here: one left behind cannot change the id, so two schematics that differ
// only in it would silently share an image.
func Schematic(s *osv1alpha1.Schematic) *schematic.Schematic {
	sch := &schematic.Schematic{}
	if s != nil {
		sch.Customization.SystemExtensions.OfficialExtensions = s.Customization.SystemExtensions.OfficialExtensions
		sch.Customization.ExtraKernelArgs = s.Customization.ExtraKernelArgs
	}
	return sch
}

// ISOURL computes the content-addressed URL of the generic secret-free install
// ISO for a schematic — offline (the id is a content hash), through talhelper's
// own URL template and defaults, so a boot medium and the install image inside
// a rendered config always name the same factory artifact. A nil schematic is
// the vanilla image.
//
// Computing a URL is not knowing that anything answers it: see Register.
func (f *Factory) ISOURL(s *osv1alpha1.Schematic, talosVersion string) (string, error) {
	return thtalos.GetImageURL(Schematic(s), f.imageFactory(), (&config.Node{}).GetMachineSpec(), talosVersion, true)
}

// Register tells the factory about a schematic, and returns the id it answers
// with.
//
// The id is a content hash anyone can compute offline, but the factory only
// serves images for schematics it has been told about: a combination nobody has
// asked for before is a 404 until someone posts it. Every stock combination is
// already registered by whoever asked for it first, so the only schematics that
// fail this way are the ones an installation authored itself — which is exactly
// when the failure is least expected and furthest from its cause.
//
// The answer is checked against the locally computed id. The two can only
// disagree if our mapping of the CRD onto the factory's schematic has drifted
// from the factory's own, and then every URL we hand out addresses an image
// built from something other than what was asked for.
func (f *Factory) Register(ctx context.Context, s *osv1alpha1.Schematic) (string, error) {
	if f == nil {
		return "", fmt.Errorf("no image factory configured")
	}
	sch := Schematic(s)
	want, err := sch.ID()
	if err != nil {
		return "", fmt.Errorf("compute schematic id: %w", err)
	}
	if _, done := f.registered.Load(want); done {
		return want, nil
	}
	body, err := sch.Marshal()
	if err != nil {
		return "", fmt.Errorf("marshal schematic %s: %w", want, err)
	}

	endpoint := f.schematicsURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("post schematic %s to %s: %w", want, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("%s answered %s registering schematic %s", endpoint, resp.Status, want)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return "", fmt.Errorf("decode the answer from %s: %w", endpoint, err)
	}
	if got.ID != want {
		return "", fmt.Errorf("%s registered the schematic as %s but it was computed locally as %s — the schematic mapping has drifted from the factory's",
			endpoint, got.ID, want)
	}

	f.registered.Store(want, struct{}{})
	return want, nil
}

// imageFactory is talhelper's view of the same factory: our host, its defaults
// for everything else (protocol, endpoint, URL templates).
func (f *Factory) imageFactory() *config.ImageFactory {
	c := &config.TalhelperConfig{}
	if f != nil {
		c.ImageFactory.RegistryURL = f.RegistryURL
	}
	return c.GetImageFactory()
}

func (f *Factory) schematicsURL() string {
	c := f.imageFactory()
	return c.Protocol + "://" + c.RegistryURL + c.SchematicEndpoint
}

func (f *Factory) httpClient() *http.Client {
	if f != nil && f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

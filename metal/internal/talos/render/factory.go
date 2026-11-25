package render

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	factoryclient "github.com/siderolabs/image-factory/pkg/client"
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
	// extensions caches the official extension catalog per Talos version;
	// a release's catalog does not change.
	extensions sync.Map
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
// ISO for a schematic, offline: the id is a content hash. A nil schematic is
// the vanilla image.
//
// Computing a URL is not knowing that anything answers it: see Register.
func (f *Factory) ISOURL(s *osv1alpha1.Schematic, talosVersion string) (string, error) {
	id, err := Schematic(s).ID()
	if err != nil {
		return "", fmt.Errorf("compute schematic id: %w", err)
	}
	return "https://" + f.host() + "/image/" + id + "/" + talosVersion + "/metal-amd64.iso", nil
}

// InstallerImage is the installer reference for a schematic: the same factory
// and the same id as ISOURL, so the boot medium and the image a rendered
// config installs from are one build.
//
// It is a tag, not a digest: the render reads no registry, so the rendered
// config names the image by tag. The factory builds an image
// from the schematic and the version, so the tag names one build in
// practice, but nothing pins it. A TalosUpgrade step pins the image it
// installs by digest (the rollout resolves it), so a fresh install and an
// upgrade to the same version can, in theory, pull different bytes.
func (f *Factory) InstallerImage(s *osv1alpha1.Schematic, talosVersion string) (string, error) {
	id, err := Schematic(s).ID()
	if err != nil {
		return "", fmt.Errorf("compute schematic id: %w", err)
	}
	return f.host() + "/metal-installer/" + id + ":" + talosVersion, nil
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

	// The factory's own client posts the same YAML body sch.Marshal gives,
	// so the id the factory computes is the one computed above.
	base := f.baseURL()
	fc, err := factoryclient.New(base, factoryclient.WithClient(*f.httpClient()))
	if err != nil {
		return "", fmt.Errorf("image factory %s: %w", base, err)
	}
	got, _, err := fc.SchematicCreate(ctx, *sch)
	if err != nil {
		return "", fmt.Errorf("register schematic %s with %s: %w", want, base, err)
	}
	if got != want {
		return "", fmt.Errorf("%s registered the schematic as %s but it was computed locally as %s — the schematic mapping has drifted from the factory's",
			base, got, want)
	}

	f.registered.Store(want, struct{}{})
	return want, nil
}

// ExtensionImages resolves official extension names (siderolabs/drbd) to the
// digest-pinned image references the factory builds the given Talos release
// with, in the order asked. The imager takes images, not names; asking the
// factory keeps an image built by the operator equal in content to the one
// the factory would serve for the same schematic.
func (f *Factory) ExtensionImages(ctx context.Context, talosVersion string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if f == nil {
		return nil, fmt.Errorf("no image factory configured to resolve extensions %v", names)
	}
	catalog, err := f.extensionCatalog(ctx, talosVersion)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(names))
	for _, n := range names {
		ref, ok := catalog[n]
		if !ok {
			return nil, fmt.Errorf("extension %q is not an official extension of Talos %s at %s", n, talosVersion, f.host())
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (f *Factory) extensionCatalog(ctx context.Context, talosVersion string) (map[string]string, error) {
	if c, ok := f.extensions.Load(talosVersion); ok {
		return c.(map[string]string), nil
	}
	base := f.baseURL()
	fc, err := factoryclient.New(base, factoryclient.WithClient(*f.httpClient()))
	if err != nil {
		return nil, fmt.Errorf("image factory %s: %w", base, err)
	}
	infos, err := fc.ExtensionsVersions(ctx, talosVersion)
	if err != nil {
		return nil, fmt.Errorf("list extensions of Talos %s at %s: %w", talosVersion, base, err)
	}
	catalog := make(map[string]string, len(infos))
	for _, i := range infos {
		ref := i.Ref
		if i.Digest != "" {
			ref += "@" + i.Digest
		}
		catalog[i.Name] = ref
	}
	f.extensions.Store(talosVersion, catalog)
	return catalog, nil
}

// host is the factory's host[:port], PublicFactory when unset.
func (f *Factory) host() string {
	if f != nil && f.RegistryURL != "" {
		return f.RegistryURL
	}
	return PublicFactory
}

func (f *Factory) baseURL() string {
	return "https://" + f.host()
}

// ImageClient is the client that downloads the images the factory serves.
// It trusts what HTTPClient trusts; without one it trusts the system CAs
// through the default transport (which honors the proxy environment), with
// a timeout long enough for an image the factory builds on request.
func (f *Factory) ImageClient() *http.Client {
	if f != nil && f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: imageTimeout}
}

// imageTimeout bounds one image download: the factory builds an image on
// its first request, and an image is a few hundred megabytes.
const imageTimeout = 30 * time.Minute

func (f *Factory) httpClient() *http.Client {
	if f != nil && f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Package oci pulls a single-layer artifact from an OCI registry by
// digest-pinned reference — the firmware-image path. A pull is two GETs
// against the distribution spec (manifest, then the one layer blob), each
// verified against its digest before use, so the executor never flashes bytes
// it cannot prove. Hand-rolled over stdlib: a full OCI client library buys
// tag resolution, auth flows and content stores this path must never need —
// references are digest-pinned by schema and the internal registry pulls
// anonymously.
package oci

import (
	"context"
	// go-digest hashes through crypto.Hash and panics on an algorithm no
	// package registered; import the one references pin to explicitly.
	_ "crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"
)

// maxBytes bounds a fetched body. BMC images run tens of MB; anything near
// this limit is a misconfigured reference, not firmware.
const maxBytes = 256 << 20

type manifest struct {
	Layers []struct {
		Digest digest.Digest `json:"digest"`
	} `json:"layers"`
}

// Pull fetches the artifact ref names (host[:port]/repo@sha256:hex) over
// HTTPS. Exactly one layer is required — the artifact IS the image, and a
// multi-layer manifest means the reference points at something else. A nil
// client uses http.DefaultClient.
func Pull(ctx context.Context, hc *http.Client, ref string) ([]byte, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	host, repo, want, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	raw, err := fetch(ctx, hc, "https://"+host+"/v2/"+repo+"/manifests/"+want.String(), want,
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	if err != nil {
		return nil, fmt.Errorf("oci: manifest %s: %w", ref, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("oci: manifest %s: %w", ref, err)
	}
	if len(m.Layers) != 1 {
		return nil, fmt.Errorf("oci: %s: want a single-layer artifact, manifest has %d layers", ref, len(m.Layers))
	}
	// The layer digest comes from the registry, not from the approved
	// reference, so validate it before it goes into a URL: a malformed one is
	// a bad manifest, not a mismatch to discover after the download.
	layer := m.Layers[0].Digest
	if err := layer.Validate(); err != nil {
		return nil, fmt.Errorf("oci: manifest %s: layer digest %q: %w", ref, layer, err)
	}
	blob, err := fetch(ctx, hc, "https://"+host+"/v2/"+repo+"/blobs/"+layer.String(), layer, "")
	if err != nil {
		return nil, fmt.Errorf("oci: blob %s: %w", ref, err)
	}
	return blob, nil
}

func parseRef(ref string) (host, repo string, d digest.Digest, err error) {
	rest, pin, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(pin, "sha256:") {
		return "", "", "", fmt.Errorf("oci: reference %q is not digest-pinned", ref)
	}
	if d, err = digest.Parse(pin); err != nil {
		return "", "", "", fmt.Errorf("oci: reference %q: %w", ref, err)
	}
	host, repo, ok = strings.Cut(rest, "/")
	if !ok || host == "" || repo == "" {
		return "", "", "", fmt.Errorf("oci: reference %q has no host/repository split", ref)
	}
	return host, repo, d, nil
}

func fetch(ctx context.Context, hc *http.Client, url string, want digest.Digest, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", url, maxBytes)
	}
	if got := want.Algorithm().FromBytes(body); got != want {
		return nil, fmt.Errorf("GET %s: digest %s, want %s", url, got, want)
	}
	return body, nil
}

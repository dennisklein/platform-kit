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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxBytes bounds a fetched body. BMC images run tens of MB; anything near
// this limit is a misconfigured reference, not firmware.
const maxBytes = 256 << 20

type manifest struct {
	Layers []struct {
		Digest string `json:"digest"`
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
	host, repo, digest, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	raw, err := fetch(ctx, hc, "https://"+host+"/v2/"+repo+"/manifests/"+digest, digest,
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
	blob, err := fetch(ctx, hc, "https://"+host+"/v2/"+repo+"/blobs/"+m.Layers[0].Digest, m.Layers[0].Digest, "")
	if err != nil {
		return nil, fmt.Errorf("oci: blob %s: %w", ref, err)
	}
	return blob, nil
}

func parseRef(ref string) (host, repo, digest string, err error) {
	rest, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return "", "", "", fmt.Errorf("oci: reference %q is not digest-pinned", ref)
	}
	host, repo, ok = strings.Cut(rest, "/")
	if !ok || host == "" || repo == "" {
		return "", "", "", fmt.Errorf("oci: reference %q has no host/repository split", ref)
	}
	return host, repo, digest, nil
}

func fetch(ctx context.Context, hc *http.Client, url, wantDigest, accept string) ([]byte, error) {
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
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != wantDigest {
		return nil, fmt.Errorf("GET %s: digest %s, want %s", url, got, wantDigest)
	}
	return body, nil
}

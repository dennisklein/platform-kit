package oci

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/opencontainers/go-digest"
)

// manifestTypes are the manifest media types a tag may point at: an index
// (a multi-platform installer) or a single manifest. The digest of whichever
// the registry serves is what the tag means.
const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// Resolve pins a tagged image reference (host/repo:tag) to the digest the
// registry serves for it now, and returns host/repo:tag@sha256:<hex>. A
// rollout plan carries the pinned form, so the image a person approved is
// the image installed even if the tag moves later. A reference that already
// carries a digest is returned as it is.
//
// Registries that answer 401 with a Bearer challenge (ghcr.io, the image
// factory) get an anonymous token first: installer images are public.
func Resolve(ctx context.Context, hc *http.Client, ref string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	tag, err := name.NewTag(ref, name.StrictValidation)
	if err != nil {
		return "", fmt.Errorf("oci: reference %q: %w", ref, err)
	}
	u := "https://" + tag.RegistryStr() + "/v2/" + tag.RepositoryStr() + "/manifests/" + tag.TagStr()
	d, err := manifestDigest(ctx, hc, u, "")
	if challenge := (*authChallenge)(nil); errors.As(err, &challenge) {
		token, terr := anonymousToken(ctx, hc, challenge.header, tag.RepositoryStr())
		if terr != nil {
			return "", fmt.Errorf("oci: resolve %s: %w", ref, terr)
		}
		d, err = manifestDigest(ctx, hc, u, token)
	}
	if err != nil {
		return "", fmt.Errorf("oci: resolve %s: %w", ref, err)
	}
	return ref + "@" + d.String(), nil
}

type authChallenge struct{ header string }

func (a *authChallenge) Error() string { return "registry asks for authentication: " + a.header }

// manifestDigest asks for the manifest a tag names, HEAD first (the digest
// header is all it needs), and hashes the body when the registry sends no
// digest header.
func manifestDigest(ctx context.Context, hc *http.Client, u, token string) (digest.Digest, error) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", manifestTypes)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return "", err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && token == "":
			if h := resp.Header.Get("WWW-Authenticate"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
				return "", &authChallenge{header: h}
			}
			return "", fmt.Errorf("%s %s: %s", method, u, resp.Status)
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("%s %s: %s", method, u, resp.Status)
		}
		if h := resp.Header.Get("Docker-Content-Digest"); h != "" {
			d, err := digest.Parse(h)
			if err != nil {
				return "", fmt.Errorf("%s %s: digest header %q: %w", method, u, h, err)
			}
			return d, nil
		}
		if method == http.MethodGet {
			if rerr != nil {
				return "", rerr
			}
			return digest.FromBytes(body), nil
		}
	}
	return "", fmt.Errorf("GET %s: no manifest", u)
}

// anonymousToken fetches a pull token from the realm a Bearer challenge
// names (the Docker token protocol), without credentials.
func anonymousToken(ctx context.Context, hc *http.Client, header, repo string) (string, error) {
	params := map[string]string{}
	for _, part := range strings.Split(header[len("bearer "):], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			params[strings.ToLower(k)] = strings.Trim(v, `"`)
		}
	}
	realm := params["realm"]
	if !strings.HasPrefix(realm, "https://") {
		return "", fmt.Errorf("token realm %q is not https", realm)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", cmp.Or(params["scope"], "repository:"+repo+":pull"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token %s: %s", realm, resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("token %s: %w", realm, err)
	}
	if t := cmp.Or(tok.Token, tok.AccessToken); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("token %s: empty token", realm)
}

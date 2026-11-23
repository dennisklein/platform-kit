package render

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"go.yaml.in/yaml/v4"
)

// BundleKey is the data key of the Secret a TalosCluster's secretsRef names.
// It matches the file name `talosctl gen secrets` writes, so the Secret can be
// created straight from that file.
const BundleKey = "secrets.yaml"

// LoadBundle decodes a secrets bundle as `talosctl gen secrets` writes it and
// validates it against the contract of talosVersion. The decode is strict: a
// misspelled key would otherwise leave a certificate empty and surface as a
// cluster that cannot form, far from its cause.
func LoadBundle(data []byte, talosVersion string) (*secrets.Bundle, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("secrets bundle is empty")
	}
	contract, err := Contract(talosVersion)
	if err != nil {
		return nil, err
	}
	bundle := &secrets.Bundle{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(bundle); err != nil {
		return nil, fmt.Errorf("decode secrets bundle: %w", err)
	}
	if err := bundle.Validate(contract); err != nil {
		return nil, fmt.Errorf("secrets bundle: %w", err)
	}
	bundle.Clock = secrets.NewClock()
	return bundle, nil
}

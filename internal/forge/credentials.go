package forge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Credentials is the file pk forge-init writes and pk bench, pk ci and pk
// fluxsim read (default <data>/forge.json, in compose a shared volume).
type Credentials struct {
	// URL is the forge base URL as seen by the reader, for example http://gitea:3000.
	URL   string `json:"url"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// Tokens maps forge user to access token (dennis, anna, jonas, mara, vera,
	// dennis-agent, anna-agent, sync-bot, pk-ci).
	Tokens map[string]string `json:"tokens"`
	// Passwords of demo users (for logging in to the forge web UI).
	Passwords map[string]string `json:"passwords,omitempty"`
	// Generation changes each time forge-init builds the state repo (also
	// on --reset); readers drop local state of an older generation.
	Generation string `json:"generation,omitempty"`
}

// CIUser is the forge user that posts the pk/policy status.
const CIUser = "pk-ci"

// LoadCredentials reads a credentials file.
func LoadCredentials(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes a credentials file with mode 0600.
func (c *Credentials) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Token returns the token of a user.
func (c *Credentials) Token(user string) (string, bool) {
	t, ok := c.Tokens[user]
	return t, ok && t != ""
}

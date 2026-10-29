package forge

import (
	"path/filepath"
	"testing"
)

func TestCredentialsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "shared", "forge.json")
	c := &Credentials{URL: "http://gitea:3000", Owner: "platform", Repo: "state", Tokens: map[string]string{"dennis": "t1"}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCredentials(p)
	if err != nil {
		t.Fatal(err)
	}
	if tok, ok := got.Token("dennis"); !ok || tok != "t1" {
		t.Fatalf("token: %q %v", tok, ok)
	}
	if _, ok := got.Token("anna"); ok {
		t.Fatal("anna has no token")
	}
}

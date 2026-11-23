package change

import (
	"strings"
	"testing"
)

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := NewID()
		if !ValidID(id) || strings.ContainsAny(id[2:], "0189") {
			t.Fatalf("invalid id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 1990 {
		t.Fatalf("too many collisions: %d unique of 2000", len(seen))
	}
	for _, old := range []string{"c-31d0aa", "c-9kq1mf", "c-0b9cfe"} {
		if !ValidID(old) {
			t.Errorf("seed id %q must be valid", old)
		}
	}
	for _, bad := range []string{"c-K7M2QZ", "c-k7m2q", "c-k7m2q_", "x-k7m2qz", "c-k7m2qz-"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Add cn-035 and cn-036 to defiant": "add-cn-035-and-cn-036-to-defiant",
		"  Bump podinfo to 6.9!  ":         "bump-podinfo-to-6-9",
		"Cabling for GC5A-6Z":              "cabling-for-gc5a-6z",
		"Ümlaut & co":                      "mlaut-co",
		"!!!":                              "change",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	long := Slug("Add a very long title that goes on and on beyond forty characters")
	if len(long) > 40 || strings.HasSuffix(long, "-") {
		t.Errorf("long slug %q", long)
	}
}

func TestBranch(t *testing.T) {
	b := Branch("c-k7m2qz", "Add cn-035 and cn-036 to defiant")
	if b != "change/c-k7m2qz-add-cn-035-and-cn-036-to-defiant" {
		t.Fatalf("branch %q", b)
	}
	id, slug, ok := ParseBranch(b)
	if !ok || id != "c-k7m2qz" || slug != "add-cn-035-and-cn-036-to-defiant" {
		t.Fatalf("parse: %q %q %v", id, slug, ok)
	}
	for _, old := range []string{"c-31d0aa", "c-9kq1mf", "c-0b9cfe"} {
		if !ValidID(old) {
			t.Errorf("seed id %q must be valid", old)
		}
	}
	for _, bad := range []string{"main", "change/c-k7m2qzx", "change/foo", "feature/c-k7m2qz-x"} {
		if _, _, ok := ParseBranch(bad); ok {
			t.Errorf("ParseBranch(%q) ok", bad)
		}
	}
}

package change

import (
	"crypto/rand"
	"regexp"
	"strings"
)

// alphabet is base32 lowercase (RFC 4648 letters and digits 2-7).
const alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// idRe accepts lowercase letters and all digits: new ids are base32, but
// ids from before (the seed history) use 0, 1, 8 and 9 too.
var idRe = regexp.MustCompile(`^c-[a-z0-9]{6}$`)

// NewID returns a random change id like c-k7m2qz.
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	out := make([]byte, 6)
	for i, v := range b {
		out[i] = alphabet[int(v)&31]
	}
	return "c-" + string(out)
}

// ValidID reports whether id is a change id.
func ValidID(id string) bool { return idRe.MatchString(id) }

const maxSlug = 40

// Slug makes a branch slug from a title: lowercase, [a-z0-9-], at most 40
// characters, cut at a dash when possible.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlug {
		s = s[:maxSlug]
		if i := strings.LastIndexByte(s, '-'); i > maxSlug/2 {
			s = s[:i]
		}
		s = strings.Trim(s, "-")
	}
	if s == "" {
		s = "change"
	}
	return s
}

// BranchPrefix is the prefix of change branches.
const BranchPrefix = "change/"

// Branch returns change/<id>-<slug>.
func Branch(id, title string) string { return BranchPrefix + id + "-" + Slug(title) }

// ParseBranch returns the id and slug of a change branch.
func ParseBranch(branch string) (id, slug string, ok bool) {
	rest, found := strings.CutPrefix(branch, BranchPrefix)
	if !found || len(rest) < 8 {
		return "", "", false
	}
	id = rest[:8]
	if !ValidID(id) {
		return "", "", false
	}
	if len(rest) == 8 {
		return id, "", true
	}
	if rest[8] != '-' {
		return "", "", false
	}
	return id, rest[9:], true
}

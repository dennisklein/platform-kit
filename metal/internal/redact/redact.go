// Package redact removes media access secrets from text that a BMC or a
// media server wrote, before the text goes into an object's status, a
// condition, an Event, a log line or the audit trail.
//
// A BMC that refuses a mount often echoes what it was given. The image URL
// is in the error body, and on the Huawei dialect the URL carries the grant
// credentials as userinfo. A Redfish service can also echo the UserName and
// Password action parameters in MessageArgs. People with read access to
// operations (oncall, approvers) read that text, and a live grant URL with its
// credentials fetches a node's machine config. So the text is cleaned at one
// place, with one set of rules, and each place that records an error calls it.
//
// The rules:
//
//   - The userinfo of a URL becomes "***" (scheme://***@host).
//   - The token of a grant path becomes "***" (/grants/***/<file>).
//   - The query of a URL becomes "***": a signed URL carries its key there.
//   - The value of a JSON member whose name says password, token or secret
//     becomes "***".
//   - Each value the caller names as a secret (a grant password the caller
//     holds) becomes "***", wherever it appears.
//
// The host and the path of a URL stay: they say which server and which
// medium, which the reader needs.
package redact

import (
	"regexp"
	"strings"
)

// Mask replaces what the rules remove.
const Mask = "***"

// minSecret is the shortest value String replaces by value. A shorter one,
// such as the media user name "media", is not secret and would mask plain
// words of the message.
const minSecret = 8

var (
	// userinfo: the part between "://" and "@" before the host. Escaped
	// slashes (":\/\/") are matched too: a JSON body escapes them.
	userinfo = regexp.MustCompile(`(://|:\\/\\/)[^/\\\s"'<>@]*@`)
	// grantToken: the path element after "grants/", with plain or escaped
	// slashes.
	grantToken = regexp.MustCompile(`(grants(?:/|\\/))[^/\\\s"'<>?#]+`)
	// query: the query of a URL, up to the end of the URL.
	query = regexp.MustCompile(`((?:https?|ftp|nfs|cifs)(?::\\?/\\?/|://)[^\s"'<>?]*)\?[^\s"'<>]*`)
	// secretMember: a JSON member such as "Password": "...", any case.
	secretMember = regexp.MustCompile(`(?i)("[a-z_]*(?:password|passwd|token|secret)[a-z_]*"\s*:\s*")(?:[^"\\]|\\.)*(")`)
)

// String returns s with the media access secrets masked. secrets are
// further values to mask wherever they appear; values shorter than eight
// characters are not masked by value.
func String(s string, secrets ...string) string {
	for _, v := range secrets {
		if len(v) >= minSecret {
			s = strings.ReplaceAll(s, v, Mask)
		}
	}
	s = userinfo.ReplaceAllString(s, "${1}"+Mask+"@")
	s = grantToken.ReplaceAllString(s, "${1}"+Mask)
	s = query.ReplaceAllString(s, "${1}?"+Mask)
	s = secretMember.ReplaceAllString(s, "${1}"+Mask+"${2}")
	return s
}

// Error wraps err so that its text is masked. errors.Is and errors.As still
// see err, so a caller can tell a timeout from a refusal. Nil stays nil.
func Error(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return &redacted{err: err, msg: String(err.Error(), secrets...)}
}

type redacted struct {
	err error
	msg string
}

func (e *redacted) Error() string { return e.msg }
func (e *redacted) Unwrap() error { return e.err }

package llm

import (
	"regexp"
	"strings"
)

// Redacted replaces a removed value in text that goes to a model.
const Redacted = "[redacted]"

// masked is the mask the bench API uses for secret values.
const masked = "••••••••"

// Filter is the redaction filter. It is a hard filter: it runs on every text
// before a model call, after the API already masked secret fields.
type Filter struct {
	// keys are lower-case field names whose values are always removed
	// (x-pk-secret fields of the schemas plus well-known secret names).
	keys map[string]bool
}

// defaultSecretKeys are field names that hold secrets in common manifests.
var defaultSecretKeys = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "api-key",
	"accesskey", "access_key", "secretkey", "secret_key", "privatekey", "private_key",
	"clientsecret", "client_secret", "authtoken", "auth_token", "bearertoken", "bearer_token",
	"secretaccesskey", "secret_access_key", "credentials", "bindpw", "encryption_key",
}

// NewRedactor returns the filter. secretKeys are extra field names (the leaf
// names of x-pk-secret schema fields), matched case-insensitively.
func NewRedactor(secretKeys ...string) *Filter {
	f := &Filter{keys: map[string]bool{}}
	for _, k := range defaultSecretKeys {
		f.keys[k] = true
	}
	for _, k := range secretKeys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			f.keys[k] = true
		}
	}
	return f
}

var (
	// SOPS values: ENC[AES256_GCM,data:...,iv:...,tag:...,type:str].
	reSOPS = regexp.MustCompile(`ENC\[[^\]\s]*\]?`)
	// PEM private keys (RSA, EC, OPENSSH, ENCRYPTED, plain PRIVATE KEY).
	rePEM = regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY( BLOCK)?-----.*?(-----END ([A-Z0-9 ]*)PRIVATE KEY( BLOCK)?-----|\z)`)
	// age identities.
	reAge = regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Z]{20,}`)
	// Authorization headers and bearer tokens.
	reBearer = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+([A-Za-z0-9._~+/\-]{16,}=*)`)
	// Token formats with a known prefix.
	reTokens = regexp.MustCompile(`\b(` +
		`pk_(?:agt|adm)_[A-Za-z0-9]{16,}` + // platform-kit agent and admin tokens
		`|sk-(?:proj-|ant-)?[A-Za-z0-9_\-]{20,}` + // OpenAI, Anthropic style keys
		`|gh[pousr]_[A-Za-z0-9]{30,}` + // GitHub
		`|github_pat_[A-Za-z0-9_]{30,}` +
		`|glpat-[A-Za-z0-9_\-]{20,}` + // GitLab
		`|xox[abprs]-[A-Za-z0-9\-]{10,}` + // Slack
		`|AKIA[0-9A-Z]{16}` + // AWS access key id
		`|hv[sb]\.[A-Za-z0-9_\-]{20,}` + // Vault / OpenBao tokens
		`|eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}` + // JWT
		`)`)
	// Credentials in URLs: scheme://user:password@host.
	reURLCred = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+:)([^\s@/]+)(@)`)
	// key: value (YAML) and "key": "value" (JSON) pairs.
	reYAMLPair = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?["']?)([A-Za-z0-9_.\-]+)(["']?\s*:[ \t]+)(.+?)\s*$`)
	reJSONPair = regexp.MustCompile(`"([A-Za-z0-9_.\-]+)"(\s*:\s*)"((?:[^"\\]|\\.)*)"`)
	reFlowPair = regexp.MustCompile(`([{,]\s*)([A-Za-z0-9_\-]+)(\s*:\s*)([^,}\s][^,}]*)`)
)

// Redact returns text without secrets and the number of removed values.
// IP addresses, MAC addresses and serial numbers stay.
func (f *Filter) Redact(text string) (string, int) {
	if text == "" {
		return text, 0
	}
	n := 0
	text, c := f.sopsBlocks(text)
	n += c
	repl := func(re *regexp.Regexp, s string, fn func(m []string) string) string {
		return re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			return fn(re.FindStringSubmatch(m))
		})
	}
	text = repl(rePEM, text, func([]string) string { return Redacted })
	text = repl(reSOPS, text, func([]string) string { return Redacted })
	text = repl(reAge, text, func([]string) string { return Redacted })
	text = repl(reTokens, text, func([]string) string { return Redacted })
	text = repl(reBearer, text, func(m []string) string { return m[1] + " " + Redacted })
	text = repl(reURLCred, text, func(m []string) string { return m[1] + Redacted + m[3] })
	text, c = f.keyValues(text)
	n += c
	return text, n
}

// keyValues removes the values of secret keys in YAML, JSON and flow maps.
func (f *Filter) keyValues(text string) (string, int) {
	n := 0
	text = reYAMLPair.ReplaceAllStringFunc(text, func(m string) string {
		s := reYAMLPair.FindStringSubmatch(m)
		if !f.secretKey(s[2]) || clean(s[4]) {
			return m
		}
		v := strings.TrimSpace(s[4])
		// A nested block (key:) or a flow map is handled by the other rules.
		if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") || strings.HasPrefix(v, "#") {
			return m
		}
		n++
		return s[1] + s[2] + s[3] + Redacted
	})
	text = reJSONPair.ReplaceAllStringFunc(text, func(m string) string {
		s := reJSONPair.FindStringSubmatch(m)
		if !f.secretKey(s[1]) || clean(s[3]) {
			return m
		}
		n++
		return `"` + s[1] + `"` + s[2] + `"` + Redacted + `"`
	})
	text = reFlowPair.ReplaceAllStringFunc(text, func(m string) string {
		s := reFlowPair.FindStringSubmatch(m)
		if !f.secretKey(s[2]) || clean(s[4]) {
			return m
		}
		n++
		return s[1] + s[2] + s[3] + Redacted
	})
	return text, n
}

// clean reports values that hold no secret: empty, already masked or
// redacted, references and placeholders.
func clean(v string) bool {
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	switch {
	case v == "", v == "~", v == "null", v == "true", v == "false", v == "|", v == ">", v == "|-", v == ">-":
		return true
	case strings.Contains(v, masked), strings.Contains(v, Redacted):
		return true
	case strings.HasPrefix(v, "${"), strings.HasPrefix(v, "{{"), strings.HasPrefix(v, "<"):
		return true
	case strings.HasPrefix(v, "secretKeyRef"), strings.HasPrefix(v, "valueFrom"):
		return true
	}
	return false
}

func (f *Filter) secretKey(k string) bool {
	k = strings.ToLower(k)
	if f.keys[k] {
		return true
	}
	// Dotted keys (hubble.ui.auth.token) count by their leaf.
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		return f.keys[k[i+1:]]
	}
	return false
}

// sopsBlocks replaces the sops metadata block of SOPS files (a "sops:" key
// with an indented body) by one line.
func (f *Filter) sopsBlocks(text string) (string, int) {
	if !strings.Contains(text, "sops:") {
		return text, 0
	}
	lines := strings.SplitAfter(text, "\n")
	var out strings.Builder
	n := 0
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		trim := strings.TrimLeft(l, " ")
		indent := len(l) - len(trim)
		if strings.TrimSpace(trim) == "sops:" {
			j := i + 1
			for j < len(lines) {
				body := lines[j]
				bt := strings.TrimLeft(body, " ")
				if strings.TrimSpace(body) != "" && len(body)-len(bt) <= indent {
					break
				}
				j++
			}
			if j > i+1 {
				n++
				out.WriteString(l[:indent] + "sops: " + Redacted + "\n")
				i = j - 1
				continue
			}
		}
		out.WriteString(l)
	}
	return out.String(), n
}

// RedactMessages redacts the contents and tool call arguments of messages in
// place and returns the number of removed values.
func RedactMessages(r Redactor, msgs []Message) int {
	if r == nil {
		return 0
	}
	n := 0
	for i := range msgs {
		var c int
		msgs[i].Content, c = r.Redact(msgs[i].Content)
		n += c
		for j := range msgs[i].ToolCalls {
			s, c := r.Redact(string(msgs[i].ToolCalls[j].Arguments))
			if c > 0 {
				msgs[i].ToolCalls[j].Arguments = []byte(s)
				n += c
			}
		}
	}
	return n
}

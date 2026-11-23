package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSOPSValue(t *testing.T) {
	in := `spec:
  values:
    hubble:
      ui:
        auth:
          token: ENC[AES256_GCM,data:Zm9vYmFy,iv:abc=,tag:def==,type:str]
`
	out, n := NewRedactor().Redact(in)
	if strings.Contains(out, "ENC[") || strings.Contains(out, "Zm9vYmFy") {
		t.Fatalf("SOPS value left:\n%s", out)
	}
	if n == 0 || !strings.Contains(out, "token: "+Redacted) {
		t.Fatalf("n=%d out:\n%s", n, out)
	}
}

func TestRedactSOPSMetadataBlock(t *testing.T) {
	in := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  interval: 5m
sops:
  age:
    - recipient: age1qyqszqgpqyqszqgpqyqszqgpqyqszqgp
      enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSBz
        -----END AGE ENCRYPTED FILE-----
  lastmodified: "2026-10-01T10:00:00Z"
  mac: ENC[AES256_GCM,data:abc,type:str]
  version: 3.9.0
---
kind: Other
`
	out, n := NewRedactor().Redact(in)
	if strings.Contains(out, "age1qy") || strings.Contains(out, "YWdl") || strings.Contains(out, "lastmodified") {
		t.Fatalf("sops block left:\n%s", out)
	}
	if !strings.Contains(out, "sops: "+Redacted+"\n---\nkind: Other") {
		t.Fatalf("block not replaced by one line:\n%s", out)
	}
	if !strings.Contains(out, "interval: 5m") {
		t.Fatalf("other fields removed:\n%s", out)
	}
	if n < 1 {
		t.Fatalf("n=%d", n)
	}
}

func TestRedactPEMAndAge(t *testing.T) {
	in := "key:\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n" +
		"rsa -----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\n" +
		"# public key: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p\n" +
		"AGE-SECRET-KEY-1QYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQY\n" +
		"cert: -----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	out, n := NewRedactor().Redact(in)
	for _, bad := range []string{"b3BlbnNzaC1rZXktdjEAAAAA", "MIIEow", "AGE-SECRET-KEY-1QYQ"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s left:\n%s", bad, out)
		}
	}
	// Public keys and certificates are not secret.
	if !strings.Contains(out, "age1ql3z7") || !strings.Contains(out, "BEGIN CERTIFICATE") {
		t.Errorf("public data removed:\n%s", out)
	}
	if n != 3 {
		t.Errorf("n=%d, want 3", n)
	}
}

func TestRedactUnterminatedPEM(t *testing.T) {
	out, _ := NewRedactor().Redact("x\n-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg")
	if strings.Contains(out, "MIIEvQ") {
		t.Fatalf("unterminated key left: %s", out)
	}
}

func TestRedactTokens(t *testing.T) {
	cases := map[string]string{
		"Authorization: Bearer pk_agt_0123456789abcdef0123456789abcdef":        "pk_agt_0123",
		"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345'":     "abcdefghijklmnop",
		"PK_TOKEN=pk_adm_0123456789abcdef0123456789abcdef0123":                 "pk_adm_0123",
		"OPENAI_API_KEY=sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz012345":              "AbCdEfGhIj",
		"gh token ghp_abcdefghijklmnopqrstuvwxyz0123456789":                    "ghp_abcdef",
		"gitlab glpat-abcdefghij0123456789xyz":                                 "glpat-abc",
		"aws AKIAIOSFODNN7EXAMPLE":                                             "AKIAIOSFODNN7",
		"vault hvs.CAESIJ0123456789abcdefghijklmnop":                           "hvs.CAES",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.SflKxwRJSMeKKF2QT4fwpM": "eyJhbGci",
		"git clone https://jonas:s3cretpassw0rd@forge.local/pk/state.git":      "s3cretpassw0rd",
	}
	f := NewRedactor()
	for in, secret := range cases {
		out, n := f.Redact(in)
		if strings.Contains(out, secret) || n == 0 {
			t.Errorf("%q -> %q (n=%d)", in, out, n)
		}
	}
	out, _ := f.Redact("git clone https://jonas:s3cretpassw0rd@forge.local/pk/state.git")
	if !strings.Contains(out, "https://jonas:"+Redacted+"@forge.local") {
		t.Errorf("url user lost: %s", out)
	}
}

func TestRedactSecretKeys(t *testing.T) {
	in := `db:
  password: hunter2hunter2
  user: admin
  apiKey: "abc123def456"
  tokenTTL: 5m
  token: ""
  secret: ••••••••
  clientSecret: ${CLIENT_SECRET}
creds: {user: x, password: opensesame1}
`
	out, n := NewRedactor("bindPassword").Redact(in)
	for _, bad := range []string{"hunter2", "abc123def456", "opensesame1"} {
		if strings.Contains(out, bad) {
			t.Errorf("%s left:\n%s", bad, out)
		}
	}
	for _, keep := range []string{"user: admin", "tokenTTL: 5m", `token: ""`, "secret: ••••••••", "clientSecret: ${CLIENT_SECRET}", "user: x"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q removed:\n%s", keep, out)
		}
	}
	if n != 3 {
		t.Errorf("n=%d, want 3:\n%s", n, out)
	}
	out, _ = NewRedactor("bindPassword").Redact("bindPassword: letmein99")
	if strings.Contains(out, "letmein99") {
		t.Errorf("extra schema key not redacted: %s", out)
	}
}

func TestRedactJSON(t *testing.T) {
	in := `{"name":"cilium","password":"p4ssw0rd!","nested":{"api_key":"k-123456"},"token":"••••••••"}`
	out, n := NewRedactor().Redact(in)
	if strings.Contains(out, "p4ssw0rd") || strings.Contains(out, "k-123456") || n != 2 {
		t.Fatalf("n=%d out=%s", n, out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("JSON broken: %v: %s", err, out)
	}
}

func TestRedactKeepsIPsSerialsAndInventory(t *testing.T) {
	in := `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: cn-035
spec:
  serial: S4R7K2J0035
  model: Supermicro SYS-220U-TNR
  bmc:
    mac: 3c:ec:ef:a1:0c:35
  ports:
    - {name: p1, cable: DATA-0201, peer: {switch: data-b, port: 37}, network: data}
neighbors:
  - peerAddress: 10.6.0.1/32
    peerASN: 65000
commit 9f2c1e4b7a0d3c5e8f1a2b3c4d5e6f708192a3b4
Pk-Change: c-2hd7vw
`
	out, n := NewRedactor().Redact(in)
	if out != in || n != 0 {
		t.Fatalf("changed (n=%d):\n%s", n, out)
	}
}

func TestRedactMessages(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "token: ENC[AES256_GCM,data:x,type:str]"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "read", Arguments: json.RawMessage(`{"q":"Bearer abcdefghijklmnopqrstuvwxyz"}`)}}},
	}
	n := RedactMessages(NewRedactor(), msgs)
	if n != 2 || strings.Contains(msgs[0].Content, "ENC[") || strings.Contains(string(msgs[1].ToolCalls[0].Arguments), "abcdefghijklmnop") {
		t.Fatalf("n=%d %+v", n, msgs)
	}
	if !json.Valid(msgs[1].ToolCalls[0].Arguments) {
		t.Fatalf("arguments not JSON: %s", msgs[1].ToolCalls[0].Arguments)
	}
}

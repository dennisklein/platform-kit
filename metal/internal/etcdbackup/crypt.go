package etcdbackup

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// ParseRecipients parses age public keys (age1..., or age1pq1... for the
// post-quantum hybrid), one per entry. Only public keys are accepted: the
// operator encrypts to them and can never decrypt what it wrote.
func ParseRecipients(keys []string) ([]age.Recipient, error) {
	if len(keys) == 0 {
		return nil, errors.New("no age recipients: name at least one public key (age1...)")
	}
	var out []age.Recipient
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if strings.HasPrefix(k, "AGE-SECRET-KEY-") {
			// Refuse with a message that says why, and never echo the key.
			return nil, errors.New("an age recipient is a secret key (AGE-SECRET-KEY-...); give its public key (age1...)," +
				" which `age-keygen -y` prints")
		}
		rs, err := age.ParseRecipients(strings.NewReader(k))
		if err != nil {
			return nil, fmt.Errorf("age recipient %q: %w", k, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

// Encrypt returns a writer that encrypts what is written to it for the
// recipients and writes the ciphertext (the binary age format) to dst. Close
// it to write the last chunk; dst is not closed.
func Encrypt(dst io.Writer, recipients []age.Recipient) (io.WriteCloser, error) {
	w, err := age.Encrypt(dst, recipients...)
	if err != nil {
		return nil, fmt.Errorf("age encrypt: %w", err)
	}
	return w, nil
}

// ParseIdentities reads age identities (secret keys) from an identity file,
// as age-keygen writes it. It runs on the admin's machine, never in the
// operator.
func ParseIdentities(r io.Reader) ([]age.Identity, error) {
	ids, err := age.ParseIdentities(r)
	if err != nil {
		return nil, fmt.Errorf("age identity file: %w", err)
	}
	return ids, nil
}

// Decrypt returns the plaintext of an age ciphertext. It fails at once when
// no identity matches; damage later in the stream surfaces as a read error,
// as age authenticates each chunk.
func Decrypt(src io.Reader, identities []age.Identity) (io.Reader, error) {
	r, err := age.Decrypt(src, identities...)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	return r, nil
}

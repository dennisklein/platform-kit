// Package identity holds the acting identity of a request: a person, an
// agent that acts for a person, or a bot.
package identity

import "context"

// Kind is the kind of an identity.
type Kind string

const (
	Human Kind = "human"
	Agent Kind = "agent"
	Bot   Kind = "bot"
)

// Identity is who makes a request.
type Identity struct {
	// User is the person, agent or bot id from the policy (dennis, dennis-agent, sync-bot).
	User string `json:"user"`
	Kind Kind   `json:"kind"`
	// ActsFor is the person an agent acts for.
	ActsFor string `json:"actsFor,omitempty"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
}

// IsHuman reports whether the identity is a person.
func (i Identity) IsHuman() bool { return i.Kind == Human }

// IsZero reports whether no identity is set.
func (i Identity) IsZero() bool { return i.User == "" }

// Principal is the person behind the identity: the user for a person, the
// person an agent acts for, and the bot itself for a bot.
func (i Identity) Principal() string {
	if i.Kind == Agent && i.ActsFor != "" {
		return i.ActsFor
	}
	return i.User
}

// CommitAuthor returns the git author name and email of the identity.
func (i Identity) CommitAuthor() (name, email string) {
	name, email = i.Name, i.Email
	if name == "" {
		name = i.User
	}
	if email == "" {
		email = i.User + "@pk.local"
	}
	return name, email
}

type ctxKey struct{}

// With returns a context that carries id.
func With(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the identity in ctx.
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok && !id.IsZero()
}

// MustFrom returns the identity in ctx or the zero identity.
func MustFrom(ctx context.Context) Identity {
	id, _ := From(ctx)
	return id
}

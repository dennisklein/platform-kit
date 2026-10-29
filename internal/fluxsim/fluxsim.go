// Package fluxsim is a simulated Flux. It follows main (and ring tags),
// renders each cluster and reports applied and verified revisions. pk fluxsim
// serves GET /status and GET /clusters/{id}/objects; the bench reads them
// through Client.
package fluxsim

import (
	"context"
	"time"
)

// Revision is a commit Flux applied or verified.
type Revision struct {
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	Change string    `json:"change,omitempty"` // Pk-Change trailer of the merge
}

// KustomizationStatus is the state of one Flux Kustomization.
type KustomizationStatus struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Ready    bool   `json:"ready"`
	Revision string `json:"revision"`
	Message  string `json:"message,omitempty"`
}

// ClusterStatus is the state of one cluster.
type ClusterStatus struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Ring string `json:"ring"`
	// Ref is what the cluster follows: main, or ring/<name> (P2).
	Ref      string    `json:"ref"`
	Applied  *Revision `json:"applied,omitempty"`
	Verified *Revision `json:"verified,omitempty"`
	// Pending is the revision Flux is about to apply.
	Pending *Revision `json:"pending,omitempty"`
	Healthy bool      `json:"healthy"`
	// Failing is a demo fault: applies fail until healed.
	Failing        bool                  `json:"failing"`
	Held           bool                  `json:"held"`
	Objects        int                   `json:"objects"`
	Kustomizations []KustomizationStatus `json:"kustomizations"`
	Message        string                `json:"message,omitempty"`
	// Twin is the cluster a digital twin emulates ("" for real clusters).
	Twin string `json:"twin,omitempty"`
	// LastApplied is when Flux applied last.
	LastApplied time.Time `json:"lastApplied,omitzero"`
}

// Status is GET /status.
type Status struct {
	// Main is the head of main fluxsim saw last.
	Main     string          `json:"main"`
	Clusters []ClusterStatus `json:"clusters"`
	// Twins are the digital twins (.pk/labs with purpose twin) that follow a ref.
	Twins []ClusterStatus `json:"twins"`
	// Rings maps ring name to the sha of its tag ring/<name> (P2).
	Rings map[string]string `json:"rings,omitempty"`
	// Error is the last fetch error ("" when the forge answered).
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

// Object is one applied object (GET /clusters/{id}/objects).
type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Kustomization that applied it.
	Kustomization string `json:"kustomization"`
	Ready         bool   `json:"ready"`
}

// Client reads fluxsim.
type Client interface {
	Status(ctx context.Context) (*Status, error)
	Objects(ctx context.Context, cluster string) ([]Object, error)
	// SetFailing injects or heals a demo fault (POST /clusters/{id}/fail).
	SetFailing(ctx context.Context, cluster string, failing bool) (*ClusterStatus, error)
}

// Options of pk fluxsim.
type Options struct {
	// Addr to listen on (GET /status, GET /clusters/{id}/objects, POST /clusters/{id}/fail).
	Addr string
	// CredentialsPath is the forge credentials file (reads main with the pk-ci token).
	CredentialsPath string
	// RemoteURL overrides the clone URL of the state repo (tests, local paths).
	RemoteURL string
	// ForgeURL, Owner and Repo name the state repo when the credentials
	// file does not (PK_FORGE_URL wins over the URL in the file).
	ForgeURL, Owner, Repo string
	// ForgeURLSet marks an explicit PK_FORGE_URL.
	ForgeURLSet bool
	// DataDir holds the clone fluxsim renders from.
	DataDir string
	// Interval between polls of main.
	Interval time.Duration
	// ApplyDelay and VerifyDelay simulate Flux timing.
	ApplyDelay  time.Duration
	VerifyDelay time.Duration
}

package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authnclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authzclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// The metrics endpoint's gate: the caller's bearer token goes to the API
// server in a TokenReview, and the user it names in a SubjectAccessReview for
// get on the non-resource URL of the request. controller-runtime ships the
// same filter (pkg/metrics/filters), but it brings k8s.io/apiserver and its
// tracing stack into the module for two API calls; this one uses the typed
// clients that client-go already has.

// metricsReviewTTL is how long an allowed token is remembered, so that a
// scrape every 30 seconds does not cost two API requests each time. A denied
// one is never remembered: a fresh binding then works on the next scrape.
const metricsReviewTTL = time.Minute

// reviewer asks the API server about one request's caller. The two methods
// are what tests replace.
type reviewer interface {
	authenticate(ctx context.Context, token string) (authenticationv1.UserInfo, bool, error)
	authorize(ctx context.Context, u authenticationv1.UserInfo, verb, path string) (bool, error)
}

type apiReviewer struct {
	authn authnclient.TokenReviewInterface
	authz authzclient.SubjectAccessReviewInterface
}

func (r apiReviewer) authenticate(ctx context.Context, token string) (authenticationv1.UserInfo, bool, error) {
	tr, err := r.authn.Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token}},
		metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.UserInfo{}, false, err
	}
	return tr.Status.User, tr.Status.Authenticated, nil
}

func (r apiReviewer) authorize(ctx context.Context, u authenticationv1.UserInfo, verb, path string) (bool, error) {
	extra := map[string]authorizationv1.ExtraValue{}
	for k, v := range u.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar, err := r.authz.Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: u.Username, UID: u.UID, Groups: u.Groups, Extra: extra,
		NonResourceAttributes: &authorizationv1.NonResourceAttributes{Path: path, Verb: verb},
	}}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return sar.Status.Allowed, nil
}

// metricsAuthFilter is a metricsserver.FilterProvider.
func metricsAuthFilter(cfg *rest.Config, hc *http.Client) (metricsserver.Filter, error) {
	authn, err := authnclient.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, err
	}
	authz, err := authzclient.NewForConfigAndClient(cfg, hc)
	if err != nil {
		return nil, err
	}
	g := &metricsGate{rev: apiReviewer{authn: authn.TokenReviews(), authz: authz.SubjectAccessReviews()}}
	return g.filter, nil
}

type metricsGate struct {
	rev reviewer
	now func() time.Time

	mu      sync.Mutex
	allowed map[[sha256.Size]byte]time.Time // token hash and verb+path → until
}

func (g *metricsGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *metricsGate) filter(log logr.Logger, next http.Handler) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "Unauthorized: send a Kubernetes bearer token", http.StatusUnauthorized)
			return
		}
		verb, path := strings.ToLower(req.Method), req.URL.Path
		key := sha256.Sum256([]byte(verb + " " + path + " " + token))
		if g.remembered(key) {
			next.ServeHTTP(w, req)
			return
		}
		user, authenticated, err := g.rev.authenticate(req.Context(), token)
		if err != nil {
			log.Error(err, "metrics: token review failed")
			http.Error(w, "token review failed", http.StatusInternalServerError)
			return
		}
		if !authenticated {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		allowed, err := g.rev.authorize(req.Context(), user, verb, path)
		if err != nil {
			log.Error(err, "metrics: access review failed", "user", user.Username)
			http.Error(w, "access review failed", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "Forbidden: "+user.Username+" may not "+verb+" "+path, http.StatusForbidden)
			return
		}
		g.remember(key)
		next.ServeHTTP(w, req)
	}), nil
}

func (g *metricsGate) remembered(key [sha256.Size]byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.allowed[key]
	return ok && g.clock().Before(until)
}

func (g *metricsGate) remember(key [sha256.Size]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	if g.allowed == nil {
		g.allowed = map[[sha256.Size]byte]time.Time{}
	}
	for k, until := range g.allowed {
		if !now.Before(until) {
			delete(g.allowed, k)
		}
	}
	g.allowed[key] = now.Add(metricsReviewTTL)
}

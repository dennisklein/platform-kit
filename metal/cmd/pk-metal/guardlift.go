package main

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// namedLift is the value of a guard-lift annotation that the caller sets for
// the reason why: "<username>: <why>", the only form admission accepts
// (config/policy/guard-lift.yaml). The username is asked of the API server,
// as approve asks it, so the record names who the cluster says the caller
// is. An empty reason is no lift: it returns "".
func namedLift(ctx context.Context, cl client.Client, why string) (string, error) {
	why = strings.TrimSpace(why)
	if why == "" {
		return "", nil
	}
	user, err := whoAmI(ctx, cl)
	if err != nil {
		return "", fmt.Errorf("the guard lift must name you: %w", err)
	}
	return metalv1alpha1.GuardLiftValue(user, why), nil
}

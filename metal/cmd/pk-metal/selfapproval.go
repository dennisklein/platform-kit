package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// approvalParams is the ConfigMap that holds the approval policies'
// parameters (config/policy/hostoperation-approval.yaml), in the operator's
// namespace.
const approvalParams = "hostoperation-approval"

// selfApproval is the site's answer to "may a person approve what they
// proposed": the selfApproval key of the approval policies' parameters.
type selfApproval struct {
	// Known is false when the parameters could not be read, which is usual:
	// an approver needs no access to the operator's ConfigMaps. Admission
	// then decides, and its refusal says why.
	Known bool
	// Value is the key's value; empty when the key is missing.
	Value string
}

// readSelfApproval reads the site's setting. Any error reading it leaves it
// unknown rather than failing the approval: the setting only lets approve
// refuse early, before a person reads a plan they cannot approve.
func readSelfApproval(ctx context.Context, cl client.Client) selfApproval {
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: approvalParams}, cm); err != nil {
		return selfApproval{}
	}
	return selfApproval{Known: true, Value: cm.Data["selfApproval"]}
}

// allowed mirrors the admission policies: a missing key means Allowed, and
// any value other than Allowed counts as Forbidden, so that a mistyped
// setting closes the gate rather than opening it.
func (s selfApproval) allowed() bool {
	return !s.Known || s.Value == "" || s.Value == "Allowed"
}

// ownRole says how the user took part in a proposal: "proposed" when it
// names them as its proposer, "asked for" when its spec.requestedBy names
// them (an edit of theirs made the operator propose it), and "" when
// neither. The approval policies treat both alike.
func ownRole(user, proposedBy string, requestedBy []string) string {
	switch {
	case user == "":
		return ""
	case proposedBy == user:
		return "proposed"
	case slices.Contains(requestedBy, user):
		return "asked for"
	}
	return ""
}

// ownProposal refuses an approval of what the user proposed or asked for
// (role, from ownRole) when the site forbids it, and says what to do
// instead. what is "operation <ns>/<name>" or "rollout <name>".
func (s selfApproval) ownProposal(what, user, role string) error {
	if s.allowed() {
		return nil
	}
	where := "ConfigMap " + operatorNamespace + "/" + approvalParams
	if s.Value == "Forbidden" {
		return fmt.Errorf("%s was %s by you (%s), and this site forbids approving your own proposal "+
			"(selfApproval: Forbidden in %s); another person must approve it", what, role, user, where)
	}
	return fmt.Errorf("%s was %s by you (%s), and selfApproval in %s is %q, which is not Allowed or "+
		"Forbidden and so counts as Forbidden; another person must approve it, or an admin sets it to "+
		"Allowed or Forbidden", what, role, user, where, s.Value)
}

// note is what approve says before it asks about the user's own proposal
// (role, from ownRole): the record will name the same person twice.
func (s selfApproval) note(what, role string) string {
	if s.Known {
		return fmt.Sprintf("You %s this %s yourself. This site allows you to approve it "+
			"(selfApproval); the record names you as requester and approver.\n", role, what)
	}
	return fmt.Sprintf("You %s this %s yourself. If this site forbids that (selfApproval), "+
		"the cluster refuses the approval and another person must approve it.\n", role, what)
}

// approvalRefused is the error a person reads when the API server refuses an
// approval. An admission policy's denial carries the policy's own message,
// which says what to do; the prefix that names the policy and its binding is
// left out. Any other error is passed on as it is.
func approvalRefused(what string, err error) error {
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return fmt.Errorf("approve %s: %w", what, err)
	}
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "denied request: "); ok {
		msg = after
	}
	return fmt.Errorf("the cluster refused the approval of %s; nothing changed: %s", what, msg)
}

package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// statusCmd renders the rack from the cluster's point of view — hosts with their
// claims and path health, endpoints, and any claim still hunting for its machine.
// Reads the kubeconfig-default cluster; no flags, no BMC connection.
func statusCmd(args []string) {
	fs := newFlags("status")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal status")
		os.Exit(2)
	}

	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx := context.Background()

	hosts := &metalv1alpha1.HostList{}
	eps := &metalv1alpha1.ManagementEndpointList{}
	claims := &metalv1alpha1.HostClaimList{}
	ops := &metalv1alpha1.HostOperationList{}
	for _, list := range []client.ObjectList{hosts, eps, claims, ops} {
		if err := cl.List(ctx, list); err != nil {
			die(err)
		}
	}
	fmt.Print(renderStatus(hosts.Items, eps.Items, claims.Items, ops.Items))
}

// renderStatus is the pure rack table: hosts first (the machines), their in-flight
// operations, endpoints (the paths), and unbound claims last — bound ones already
// show on their host row.
func renderStatus(hosts []metalv1alpha1.Host, eps []metalv1alpha1.ManagementEndpoint, claims []metalv1alpha1.HostClaim, ops []metalv1alpha1.HostOperation) string {
	hosts, eps, claims, ops = slices.Clone(hosts), slices.Clone(eps), slices.Clone(claims), slices.Clone(ops)
	slices.SortFunc(hosts, func(a, b metalv1alpha1.Host) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(eps, func(a, b metalv1alpha1.ManagementEndpoint) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(claims, func(a, b metalv1alpha1.HostClaim) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	slices.SortFunc(ops, func(a, b metalv1alpha1.HostOperation) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 2, 0, 2, ' ', 0)

	fmt.Fprintln(w, "HOST\tVENDOR\tPHASE\tCLAIM\tPATHS\tBLOCKED")
	for _, h := range hosts {
		vendor, claim := "", ""
		if h.Status.Identity != nil {
			vendor = h.Status.Identity.Vendor
		}
		if ref := h.Spec.ClaimRef; ref != nil {
			claim = ref.Namespace + "/" + ref.Name
		}
		up := 0
		for _, p := range h.Status.ManagedBy {
			if p.Reachable {
				up++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\t%s\n",
			h.Name, orDash(vendor), h.Status.Phase, orDash(claim),
			up, len(h.Status.ManagedBy), h.Status.BlockedOn)
	}

	// Succeeded operations are history, not state of the rack; failures stay
	// visible until someone deletes them.
	pending := slices.DeleteFunc(ops, func(op metalv1alpha1.HostOperation) bool {
		return op.Status.Phase == metalv1alpha1.OperationSucceeded
	})
	if len(pending) > 0 {
		fmt.Fprintln(w, "\nOPERATION\tHOST\tPHASE\tAPPROVED\tBLOCKED")
		for _, op := range pending {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\n",
				op.Namespace, op.Name, op.Spec.HostRef.Name, op.Status.Phase,
				orDash(op.Spec.Approval.ApprovedBy), op.Status.BlockedOn)
		}
	}

	fmt.Fprintln(w, "\nENDPOINT\tVENDOR\tPHASE\tADDRESS\tBLOCKED")
	for _, ep := range eps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			ep.Name, orDash(ep.Status.Vendor), ep.Status.Phase, ep.Spec.Address, ep.Status.BlockedOn)
	}

	unbound := slices.DeleteFunc(claims, func(c metalv1alpha1.HostClaim) bool {
		return c.Status.Phase == metalv1alpha1.ClaimBound
	})
	if len(unbound) > 0 {
		fmt.Fprintln(w, "\nCLAIM\tASSET\tPHASE\tBLOCKED")
		for _, c := range unbound {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\n",
				c.Namespace, c.Name, c.Spec.AssetRef.Name, c.Status.Phase, c.Status.BlockedOn)
		}
	}

	_ = w.Flush()
	return sb.String()
}

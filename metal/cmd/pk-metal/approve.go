package main

import (
	"context"
	"fmt"
	"os"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// approveCmd stamps spec.approval.approvedBy with the caller's own cluster
// username — the only value the ValidatingAdmissionPolicy accepts (approve as
// yourself, write-once). Who YOU are is asked of the apiserver
// (SelfSubjectReview), not guessed from the environment.
func approveCmd(args []string) {
	fs := newFlags("approve")
	namespace := fs.String("n", "default", "namespace of the operation")
	addGlobalFlags(fs)
	parseFlags(fs, args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal approve [-n namespace] <operation-name>")
		os.Exit(2)
	}
	name := fs.Arg(0)

	cl, err := newClient()
	if err != nil {
		die(err)
	}
	ctx := context.Background()

	ssr := &authenticationv1.SelfSubjectReview{}
	if err := cl.Create(ctx, ssr); err != nil {
		dief("who am I: %v", err)
	}
	user := ssr.Status.UserInfo.Username

	op := &metalv1alpha1.HostOperation{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: *namespace, Name: name}, op); err != nil {
		die(err)
	}
	if op.Spec.Approval.ApprovedBy != "" {
		dief("operation %s/%s is already approved by %s (approval is write-once)",
			*namespace, name, op.Spec.Approval.ApprovedBy)
	}
	// Admission refuses this too; saying it here names the rule instead of
	// quoting a policy.
	if op.Spec.ProposedBy == user {
		dief("operation %s/%s was proposed by you (%s) — another person must approve it",
			*namespace, name, user)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"approval":{"approvedBy":%q}}}`, user))
	if err := cl.Patch(ctx, op, client.RawPatch(types.MergePatchType, patch)); err != nil {
		die(err)
	}
	fmt.Printf("approved %s/%s as %s — host %s\n", *namespace, name, user, op.Spec.HostRef.Name)
}

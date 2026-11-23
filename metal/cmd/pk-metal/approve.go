package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// approveCmd stamps spec.approval.approvedBy with the caller's own cluster
// username — the only value the ValidatingAdmissionPolicy accepts (approve as
// yourself, write-once). Who YOU are is asked of the apiserver
// (SelfSubjectReview), not guessed from the environment.
func approveCmd(args []string) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	namespace := fs.String("n", "default", "namespace of the operation")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: pk-metal approve [-n namespace] <operation-name>")
		os.Exit(2)
	}
	name := fs.Arg(0)

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		log.Fatal(err)
	}
	if err := metalv1alpha1.AddToScheme(s); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	ssr := &authenticationv1.SelfSubjectReview{}
	if err := cl.Create(ctx, ssr); err != nil {
		log.Fatalf("who am I: %v", err)
	}
	user := ssr.Status.UserInfo.Username

	op := &metalv1alpha1.HostOperation{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: *namespace, Name: name}, op); err != nil {
		log.Fatal(err)
	}
	if op.Spec.Approval.ApprovedBy != "" {
		log.Fatalf("operation %s/%s is already approved by %s (approval is write-once)",
			*namespace, name, op.Spec.Approval.ApprovedBy)
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"approval":{"approvedBy":%q}}}`, user))
	if err := cl.Patch(ctx, op, client.RawPatch(types.MergePatchType, patch)); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("approved %s/%s as %s — host %s\n", *namespace, name, user, op.Spec.HostRef.Name)
}

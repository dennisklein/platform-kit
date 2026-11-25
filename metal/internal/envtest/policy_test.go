package envtest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// installApprovalPolicy loads the committed approval policy — the gate under
// test is the one in config/policy, not a test-local copy — with its params
// naming operator as the operator's identity (the manager in these suites runs
// as the envtest admin, not as the deployed service account).
func installApprovalPolicy(t *testing.T, ctx context.Context, cl client.Client, operator string) {
	t.Helper()
	docs := strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", "hostoperation-approval.yaml")), "\n---")
	require.Len(t, docs, 3)
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), vap))
	require.NoError(t, cl.Create(ctx, vap))
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), binding))
	require.NoError(t, cl.Create(ctx, binding))
	params := &corev1.ConfigMap{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[2]), params))
	params.Data["operator"] = operator

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: params.Namespace}}
	if err := cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	require.NoError(t, cl.Create(ctx, params))
}

// installSettings creates the site settings ConfigMap
// (pk-metal/hostoperation-approval) from the committed one, naming operator
// as the operator, unless it exists: the policies that take it as params
// deny everything without it.
func installSettings(t *testing.T, ctx context.Context, cl client.Client, operator string) {
	t.Helper()
	docs := strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", "hostoperation-approval.yaml")), "\n---")
	require.Len(t, docs, 3)
	params := &corev1.ConfigMap{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[2]), params))
	params.Data["operator"] = operator
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: params.Namespace}}
	if err := cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	if err := cl.Create(ctx, params); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// installClaimPolicy loads the committed claim provisioning policy; its params
// are the ones installApprovalPolicy created.
func installClaimPolicy(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	docs := strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", "hostclaim-provisioning.yaml")), "\n---")
	require.Len(t, docs, 2)
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), vap))
	require.NoError(t, cl.Create(ctx, vap))
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), binding))
	require.NoError(t, cl.Create(ctx, binding))
}

// whoAmI is the username a client acts as.
func whoAmI(t *testing.T, ctx context.Context, cl client.Client) string {
	t.Helper()
	ssr := &authenticationv1.SelfSubjectReview{}
	require.NoError(t, cl.Create(ctx, ssr))
	return ssr.Status.UserInfo.Username
}

package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// scaleFleet is cluster lab with control planes cp-1..cp-3 and worker w-1
// claimed, TalosNodes cp-4 and w-2 written ahead, and free hosts h-8, h-9, h-10.
func scaleFleet() []client.Object {
	objs := []client.Object{&osv1alpha1.TalosCluster{ObjectMeta: metav1.ObjectMeta{Name: "lab"},
		Spec: osv1alpha1.TalosClusterSpec{TalosVersion: "v1.14.6"}}}
	node := func(name string, role osv1alpha1.NodeRole) {
		objs = append(objs, &osv1alpha1.TalosNode{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: osv1alpha1.TalosNodeSpec{ClusterRef: osv1alpha1.Reference{Name: "lab"}, Role: role}})
	}
	claimed := func(name, host string) {
		c := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		c.Spec.OS = &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: name}}
		c.Status.HostRef = &metalv1alpha1.Reference{Name: host}
		objs = append(objs, c)
	}
	host := func(name string) {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: name}}
		h.Status.Phase = metalv1alpha1.HostAvailable
		// The endpoint is named after its Asset.
		h.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "asset-" + name}, SystemID: "1"}}
		objs = append(objs, h)
	}
	for i, n := range []string{"cp-1", "cp-2", "cp-3"} {
		node(n, osv1alpha1.RoleControlPlane)
		claimed(n, "h-"+string(rune('1'+i)))
	}
	node("w-1", osv1alpha1.RoleWorker)
	claimed("w-1", "h-4")
	node("cp-4", osv1alpha1.RoleControlPlane)
	node("w-2", osv1alpha1.RoleWorker)
	host("h-8")
	host("h-9")
	host("h-10")
	// The machine the TalosNode w-2 was written for: its asset has the name.
	objs[len(objs)-1].(*metalv1alpha1.Host).Status.ManagedBy[0].EndpointRef.Name = "w-2"
	dirty := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: "h-7",
		Annotations: map[string]string{metalv1alpha1.AnnotationDiskNotWiped: "lab/old"}}}
	dirty.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: "asset-h-7"}, SystemID: "1"}}
	dirty.Status.Phase = metalv1alpha1.HostAvailable
	return append(objs, dirty)
}

func TestScalePlanGrows(t *testing.T) {
	ctx := context.Background()
	cl := fakeClientAs("alice", scaleFleet()...)
	p, err := planScale(ctx, cl, "lab", 4, 2, "default")
	require.NoError(t, err)
	var out bytes.Buffer
	p.print(&out)
	assert.Equal(t, `cluster lab: 3 control plane(s) and 1 worker(s) now, 4 and 2 wanted
warning: 4 control planes is an even number: it tolerates no more failures than 3
 1. add controlplane cp-4 on host h-8: claim default/cp-4; joins etcd once every member is healthy and the one before has joined
 2. add worker w-2 on host h-10: claim default/w-2; installed
check: each added TalosNode's install disk and network links must fit the host it is paired with
`, out.String(), "the host that is not wiped is not used")

	out.Reset()
	require.NoError(t, p.apply(ctx, cl, "", &out))
	c := &metalv1alpha1.HostClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "cp-4"}, c))
	assert.Equal(t, "asset-h-8", c.Spec.AssetRef.Name)
	assert.Equal(t, "1", c.Spec.SystemRef.Name)
	assert.Equal(t, "cp-4", c.Spec.OS.NodeRef.Name)
	assert.Equal(t, "alice", c.Annotations[metalv1alpha1.AnnotationRequestedBy], "the claim names who asked for it")
}

func TestScalePlanShrinksThroughAnEvenCountOnlyWhenAcknowledged(t *testing.T) {
	ctx := context.Background()
	cl := fakeClientAs("alice", scaleFleet()...)
	p, err := planScale(ctx, cl, "lab", 1, 0, "default")
	require.NoError(t, err)
	var out bytes.Buffer
	p.print(&out)
	assert.Contains(t, out.String(), " 1. remove w-1 from host h-4")
	assert.Contains(t, out.String(), " 2. remove cp-3 from host h-3")
	assert.Contains(t, out.String(), " 3. remove cp-2 from host h-2")
	assert.Contains(t, out.String(), "even number of them")

	assert.ErrorContains(t, p.apply(ctx, cl, "", &out), "--ack-etcd-risk")
	require.NoError(t, p.apply(ctx, cl, "scale down", &out))
	claims := &metalv1alpha1.HostClaimList{}
	require.NoError(t, cl.List(ctx, claims))
	var left []string
	for _, c := range claims.Items {
		left = append(left, c.Name)
	}
	assert.Equal(t, []string{"cp-1"}, left)

	_, err = planScale(ctx, cl, "lab", 0, 0, "default")
	assert.ErrorContains(t, err, "at least one control plane")
}

func TestScalePlanSaysWhatIsMissing(t *testing.T) {
	p, err := planScale(context.Background(), fakeClient(scaleFleet()...), "lab", 5, 1, "default")
	require.NoError(t, err)
	var out bytes.Buffer
	p.print(&out)
	assert.Contains(t, out.String(), "cannot: 1 more controlplane TalosNode(s) of cluster lab are needed")
	assert.Error(t, p.apply(context.Background(), fakeClient(), "", &out))
}

func TestReinstallBindsTheRequestToTheInstall(t *testing.T) {
	ctx := context.Background()
	at := metav1.NewTime(time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC))
	c := &metalv1alpha1.HostClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c-2"}}
	c.Spec.OS = &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "cp-2"}}
	cl := fakeClient(c)
	var out bytes.Buffer
	assert.ErrorContains(t, reinstall(ctx, cl, "default", "cp-2", "", &out), "records no install")

	c.Status.HandOff = &metalv1alpha1.ClaimHandOff{Host: "h-2", Node: "cp-2", At: at}
	cl = fakeClientAs("alice", c)
	require.NoError(t, reinstall(ctx, cl, "default", "cp-2", "disk swap", &out), "found by its node's name")
	got := &metalv1alpha1.HostClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(c), got))
	assert.Equal(t, "2026-11-02T03:04:05Z", got.Annotations[metalv1alpha1.AnnotationReinstall])
	assert.Equal(t, "alice: disk swap", got.Annotations[metalv1alpha1.AnnotationAckEtcdRisk], "the ack names who gave it")
	assert.Equal(t, "alice", got.Annotations[metalv1alpha1.AnnotationRequestedBy], "the request names who asked for it")
}

func TestWipeAndReplace(t *testing.T) {
	ctx := context.Background()
	cl := fakeClientAs("alice", scaleFleet()...)
	var out bytes.Buffer
	assert.ErrorContains(t, wipe(ctx, cl, "h-8", "", &out), "not marked")
	require.NoError(t, wipe(ctx, cl, "h-7", "", &out))
	h := &metalv1alpha1.Host{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "h-7"}, h))
	assert.Equal(t, "v1.14.6", h.Annotations[metalv1alpha1.AnnotationWipe], "the version of the node's cluster")
	assert.Equal(t, "alice", h.Annotations[metalv1alpha1.AnnotationRequestedBy], "the wipe names who asked for it")

	assert.ErrorContains(t, replace(ctx, cl, "default", "cp-2", "h-7", "", &out), "not wiped")
	require.NoError(t, replace(ctx, cl, "default", "cp-2", "h-9", "failed PSU", &out))
	c := &metalv1alpha1.HostClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Namespace: "default", Name: "cp-2"}, c))
	assert.Equal(t, "asset-h-9", c.Spec.AssetRef.Name)
	assert.Equal(t, "alice: failed PSU", c.Annotations[metalv1alpha1.AnnotationAckEtcdRisk], "the ack names who gave it")
	assert.Equal(t, "alice", c.Annotations[metalv1alpha1.AnnotationRequestedBy], "the move names who asked for it")
}

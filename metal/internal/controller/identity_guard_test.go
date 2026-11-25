package controller

import (
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
)

// TestIdentityPushWaitsForOperationsBehindTheEndpoint: the push restarts the
// manager, which drops the media an operation mounted. It waits while an
// approved, unfinished operation runs on a host behind the endpoint; one that
// still waits for a person, or one on another endpoint's host, holds nothing.
func TestIdentityPushWaitsForOperationsBehindTheEndpoint(t *testing.T) {
	behind := func(host, ep string) *metalv1alpha1.Host {
		h := &metalv1alpha1.Host{ObjectMeta: metav1.ObjectMeta{Name: host}}
		h.Status.ManagedBy = []metalv1alpha1.ManagementPath{{EndpointRef: metalv1alpha1.Reference{Name: ep}, SystemID: "1"}}
		return h
	}
	op := func(name, host string, approved bool) *metalv1alpha1.HostOperation {
		o := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: name},
			Spec: metalv1alpha1.HostOperationSpec{HostRef: metalv1alpha1.Reference{Name: host}, ProposedBy: "pk-metal",
				Approval: metalv1alpha1.OperationApproval{Required: ptr.To(true)},
				Actions:  []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}}},
		}
		if approved {
			o.Spec.Approval.ApprovedBy = "person"
		}
		return o
	}
	ep := &metalv1alpha1.ManagementEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "bmc-1"}}

	for name, tc := range map[string]struct {
		objs []client.Object
		want string
	}{
		"nothing behind the endpoint": {
			objs: []client.Object{behind("host-b", "bmc-2"), op("boot-b", "host-b", true)},
		},
		"an operation waiting for a person": {
			objs: []client.Object{behind("host-a", "bmc-1"), op("boot-a", "host-a", false)},
		},
		"an approved operation on a host behind it": {
			objs: []client.Object{behind("host-a", "bmc-1"), op("boot-a", "host-a", true)},
			want: "operation pk-metal/boot-a runs on host host-a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tc.objs...).Build()
			r := &EndpointConvergenceReconciler{Client: cl}
			got, err := r.operationBehind(t.Context(), ep)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRemountAfterAManagerRestart: the grant is still live in this process,
// but a manager restart emptied the slot between the recorded mount and the
// power cycle. The medium is mounted again before the machine boots, instead
// of booting an empty slot and failing at the check-in half an hour later.
func TestRemountAfterAManagerRestart(t *testing.T) {
	iso := buildISO(t, "version: v1alpha1\n")
	ba, artifact := configAsset(t, iso)
	op := preClearedOp(
		insertAsset(ba.Name, godigest.FromBytes(iso).String()),
		metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}},
	)
	op.Status = resumedStatus(1)
	op.Status.Steps[0].Action = "insertMedia"
	op.Status.OffObserved = nil
	r, st, rec := executorFor(t, "supermicro-x12", op, ba, artifact)
	r.Media = startMedia(t)

	// The mount happened in this process: its grant is live.
	creds, err := asset.MintCredentials()
	require.NoError(t, err)
	_, err = r.Media.Grants.Issue(grantHandle(client.ObjectKeyFromObject(op)), ba.Name+".iso", iso, time.Hour, creds)
	require.NoError(t, err)
	// The manager restarted: the slot is empty.
	require.Empty(t, mounted(t.Context(), st, mediaSlot))

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	assert.Len(t, rec.inserts, 1, "mounted again")
	assert.Equal(t, iso, st.BootImage(mediaSlot), "the machine booted the config, not an empty slot")
}

// TestRemountOfAPlainImage: an image named by URL is put back too, under a
// new grant.
func TestRemountOfAPlainImage(t *testing.T) {
	srv := isoServer(t)
	want := srv.URL + "/installer.iso"
	op := preClearedOp(insertURL(want), metalv1alpha1.HostOperationAction{PowerCycle: &metalv1alpha1.PowerCycleAction{}})
	op.Status = resumedStatus(1)
	op.Status.Steps[0].Action = "insertMedia"
	op.Status.OffObserved = nil
	r, st, rec := executorFor(t, "supermicro-x12", op)
	withImages(t, r, srv)
	downloaded(t, r, want, isoDigest)

	got := runOp(t, r)
	require.True(t, opSucceeded(got), "%+v", got.Status)
	require.Len(t, rec.inserts, 1)
	assert.Contains(t, rec.inserts[0], urlGrantFile(isoDigest), "mounted again, from the media service")
	assert.Equal(t, isoImage, st.BootImage(mediaSlot))
}

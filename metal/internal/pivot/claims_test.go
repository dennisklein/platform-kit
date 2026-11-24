package pivot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const storeClaim = "pk-metal-media-store"

// storeObjects is the operator as the media-store component leaves it: the
// manager mounts a claim, which was bound on the source to a volume of the
// source's storage class.
func storeObjects() []client.Object {
	objs := operatorObjects(1, nil)
	d := objs[1].(*appsv1.Deployment)
	spec := &d.Spec.Template.Spec
	spec.Volumes = []corev1.Volume{
		{Name: "media-store", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: storeClaim}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	spec.Containers[0].Args = append(spec.Containers[0].Args,
		"--media-store=/var/lib/pk-metal/media", "--media-store-claim="+storeClaim)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: storeClaim, Namespace: "pk-metal",
			Labels:      map[string]string{"app.kubernetes.io/component": "media-store"},
			Annotations: map[string]string{"pv.kubernetes.io/bind-completed": "yes", "volume.kubernetes.io/selected-node": "kind-control-plane"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			StorageClassName: ptr.To("standard"),
			VolumeName:       "pvc-0123",
			Selector:         &metav1.LabelSelector{MatchLabels: map[string]string{"disk": "fast"}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	return append(objs, pvc)
}

func storageClass(name string, isDefault bool) *storagev1.StorageClass {
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "example.com/disk"}
	if isDefault {
		sc.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return sc
}

func TestClaimMoves(t *testing.T) {
	ctx := context.Background()

	t.Run("a target with a default class gets a copy of the request", func(t *testing.T) {
		m := fakeMover(t, storeObjects(), []client.Object{storageClass("local-path", true)})
		claims, err := m.claimMoves(ctx)
		require.NoError(t, err)
		require.Len(t, claims, 1)
		c := claims[0]
		assert.Equal(t, ClaimCreate, c.Action)
		assert.Equal(t, "media-store", c.Volume)
		assert.Empty(t, c.StorageClass, "the source's class is not on the target")
		assert.Contains(t, c.String(), "default StorageClass local-path")
		assert.Contains(t, c.String(), "10Gi")

		pvc := targetClaim(c)
		assert.Equal(t, storeClaim, pvc.Name)
		assert.Equal(t, "pk-metal", pvc.Namespace)
		assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
		assert.Equal(t, resource.MustParse("10Gi"), pvc.Spec.Resources.Requests[corev1.ResourceStorage])
		assert.Equal(t, corev1.PersistentVolumeFilesystem, *pvc.Spec.VolumeMode)
		assert.Nil(t, pvc.Spec.StorageClassName, "the target's default decides")
		assert.Empty(t, pvc.Spec.VolumeName, "the source's volume stays there")
		assert.Nil(t, pvc.Spec.Selector)
		assert.Empty(t, pvc.Annotations, "the source's binding records stay there")
		assert.Equal(t, "media-store", pvc.Labels["app.kubernetes.io/component"])
	})
	t.Run("a target with the source's class keeps it", func(t *testing.T) {
		m := fakeMover(t, storeObjects(), []client.Object{storageClass("standard", false)})
		claims, err := m.claimMoves(ctx)
		require.NoError(t, err)
		require.Len(t, claims, 1)
		assert.Equal(t, ClaimCreate, claims[0].Action)
		assert.Equal(t, "standard", *targetClaim(claims[0]).Spec.StorageClassName)
	})
	t.Run("a claim the target has bound is used", func(t *testing.T) {
		there := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storeClaim, Namespace: "pk-metal"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		m := fakeMover(t, storeObjects(), []client.Object{there})
		claims, err := m.claimMoves(ctx)
		require.NoError(t, err)
		require.Len(t, claims, 1)
		assert.Equal(t, ClaimUse, claims[0].Action)
	})
	t.Run("a pending claim of a class the target has will bind", func(t *testing.T) {
		there := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storeClaim, Namespace: "pk-metal"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("local-path")},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
		m := fakeMover(t, storeObjects(), []client.Object{there, storageClass("local-path", false)})
		claims, err := m.claimMoves(ctx)
		require.NoError(t, err)
		assert.Equal(t, ClaimUse, claims[0].Action)
	})
	t.Run("a pending claim nothing provisions is refused", func(t *testing.T) {
		there := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: storeClaim, Namespace: "pk-metal"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
		m := fakeMover(t, storeObjects(), []client.Object{there})
		_, err := m.claimMoves(ctx)
		require.ErrorContains(t, err, "is Pending and names no StorageClass")
	})
	t.Run("a target without storage is refused, naming both ways out", func(t *testing.T) {
		m := fakeMover(t, storeObjects(), nil)
		_, err := m.claimMoves(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PersistentVolumeClaim "+storeClaim)
		assert.Contains(t, err.Error(), "storageclass.kubernetes.io/is-default-class=true")
		assert.Contains(t, err.Error(), "--media-store=ephemeral")

		_, err = m.Preflight(ctx)
		require.ErrorContains(t, err, "--media-store=ephemeral", "the preflight refuses before anything stops")
	})
	t.Run("ephemeral needs no storage", func(t *testing.T) {
		m := fakeMover(t, storeObjects(), nil)
		m.Options.Claims = ClaimsEphemeral
		plan, err := m.Preflight(ctx)
		require.NoError(t, err)
		require.Len(t, plan.Claims, 1)
		assert.Equal(t, ClaimEphemeral, plan.Claims[0].Action)
		assert.Contains(t, plan.Claims[0].String(), "emptyDir")
	})
	t.Run("a manager without claims has nothing to decide", func(t *testing.T) {
		m := fakeMover(t, operatorObjects(1, nil), nil)
		claims, err := m.claimMoves(ctx)
		require.NoError(t, err)
		assert.Empty(t, claims)
	})
}

func TestManagerForTargetMakesAClaimEphemeral(t *testing.T) {
	src := storeObjects()[1].(*appsv1.Deployment)
	pvc := storeObjects()[2].(*corev1.PersistentVolumeClaim)
	eph := []ClaimMove{{Volume: "media-store", Name: storeClaim, Action: ClaimEphemeral, source: pvc}}

	d := managerForTarget(src, "", "", eph)
	vols := d.Spec.Template.Spec.Volumes
	require.Len(t, vols, 2)
	assert.Nil(t, vols[0].PersistentVolumeClaim)
	require.NotNil(t, vols[0].EmptyDir)
	assert.Equal(t, resource.MustParse("10Gi"), *vols[0].EmptyDir.SizeLimit, "bounded by what the claim asked for")
	assert.Equal(t, "tmp", vols[1].Name, "other volumes stay")
	args := d.Spec.Template.Spec.Containers[0].Args
	assert.Contains(t, args, "--media-store=/var/lib/pk-metal/media", "the store is still mounted where the manager looks")
	assert.NotContains(t, args, "--media-store-claim="+storeClaim, "the imager Jobs cannot mount an emptyDir of another pod")
	assert.NotNil(t, src.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim, "the source object is not changed")

	kept := []ClaimMove{{Volume: "media-store", Name: storeClaim, Action: ClaimCreate, source: pvc}}
	d = managerForTarget(src, "", "", kept)
	assert.Equal(t, storeClaim, d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	assert.Contains(t, d.Spec.Template.Spec.Containers[0].Args, "--media-store-claim="+storeClaim,
		"a copied claim keeps its name, so the imager Jobs mount the claim the manager does")
}

func TestWaitRunningNamesAnUnboundClaim(t *testing.T) {
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: storeClaim, Namespace: "pk-metal"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	m := fakeMover(t, nil, append(operatorObjects(1, nil), pending))
	m.Options.Poll = time.Millisecond
	m.Options.Timeout = 20 * time.Millisecond
	err := m.WaitRunning(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "to run and lead")
	assert.Contains(t, err.Error(), "PersistentVolumeClaim pk-metal/"+storeClaim+" is Pending")
	assert.Contains(t, err.Error(), "kubectl -n pk-metal describe pvc "+storeClaim)
}

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/ledger"
)

func TestAssetSaysWhyItHasNoEndpoint(t *testing.T) {
	led, err := ledger.Parse([]byte(`
"0C-C4-7A-12-34-56": https://10.0.0.21
"0c:c4:7a:12:34:57": http://10.0.0.22
`))
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		mac      string
		status   metav1.ConditionStatus
		reason   string
		contains []string
	}{
		"listed in another form": {mac: "0c:c4:7a:12:34:56", status: metav1.ConditionTrue,
			reason: metalv1alpha1.ReasonEndpointMaterialized, contains: []string{"https://10.0.0.21"}},
		"not listed": {mac: "0c:c4:7a:12:34:99", status: metav1.ConditionFalse,
			reason:   metalv1alpha1.ReasonNoLedgerEntry,
			contains: []string{"0c:c4:7a:12:34:99", "the test ledger", "restart"}},
		"listed on a bad line": {mac: "0C:C4:7A:12:34:57", status: metav1.ConditionFalse,
			reason: metalv1alpha1.ReasonLedgerEntryInvalid, contains: []string{"not an https URL"}},
		"no mac at all": {status: metav1.ConditionFalse,
			reason: metalv1alpha1.ReasonNoBMCMac, contains: []string{"spec.bmcMac"}},
	} {
		t.Run(name, func(t *testing.T) {
			asset := &metalv1alpha1.Asset{
				ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
				Spec: metalv1alpha1.AssetSpec{CID: "A-1", Serial: "S1", BMCMac: tc.mac,
					CredentialsRef: corev1.LocalObjectReference{Name: "creds"}},
			}
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithStatusSubresource(&metalv1alpha1.Asset{}).WithObjects(asset).Build()
			r := &AssetReconciler{Client: cl, Scheme: cl.Scheme(), Ledger: led,
				LedgerSource: "the test ledger", LedgerReload: "Then restart the manager.",
				Recorder: events.NewFakeRecorder(10)}

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(asset)})
			require.NoError(t, err)
			got := &metalv1alpha1.Asset{}
			require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(asset), got))
			c := meta.FindStatusCondition(got.Status.Conditions, metalv1alpha1.ConditionMaterialized)
			require.NotNil(t, c)
			assert.Equal(t, tc.status, c.Status)
			assert.Equal(t, tc.reason, c.Reason)
			for _, s := range tc.contains {
				assert.Contains(t, c.Message, s)
			}
			ep := &metalv1alpha1.ManagementEndpoint{}
			epErr := cl.Get(t.Context(), client.ObjectKey{Name: "node-1"}, ep)
			assert.Equal(t, tc.status == metav1.ConditionTrue, epErr == nil, "an endpoint exists only when materialized")
		})
	}
}

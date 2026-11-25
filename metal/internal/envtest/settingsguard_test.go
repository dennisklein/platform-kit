// The settings-guard suite: only the admin group changes or deletes the site
// settings (config/policy/settings-guard.yaml), and the operator key names a
// service account.
package envtest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSettingsGuard(t *testing.T) {
	st := startEnv(t)
	cl := st.cl
	ctx := t.Context()

	installApprovalPolicy(t, ctx, cl, "system:serviceaccount:pk-metal:pk-metal-manager")
	installPolicy(t, ctx, cl, "settings-guard.yaml")
	alice := st.userClient(t, "alice")
	ada := st.userClient(t, "ada", adminGroup)
	settings := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: "pk-metal", Name: "hostoperation-approval"}
	require.NoError(t, cl.Get(ctx, key, settings))
	patch := func(c client.Client, body string) error {
		return c.Patch(ctx, settings.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(body)))
	}

	// A change that passes before the policy enforces is no change on the
	// next try: each try sets a new value.
	var last error
	try := 0
	require.Eventually(t, func() bool {
		try++
		last = patch(alice, fmt.Sprintf(`{"data":{"retention":"%dd"}}`, try))
		return last != nil && strings.Contains(last.Error(), `only a member of the admin group "pk-metal:admins"`)
	}, 30*time.Second, 100*time.Millisecond, "a person outside the admin group changed the settings (last: %v)", last)

	require.NoError(t, patch(alice, `{"metadata":{"labels":{"team":"a"}}}`), "a change that leaves data as it is passes")
	require.NoError(t, patch(ada, `{"data":{"selfApproval":"Forbidden"}}`), "the admin group changes the settings")

	err := patch(ada, `{"data":{"operator":"ada"}}`)
	require.Error(t, err, "the operator key never names a person")
	assert.Contains(t, err.Error(), "must name the manager's service account")

	require.Error(t, alice.Delete(ctx, settings.DeepCopy()), "a person outside the admin group cannot delete them")
	require.NoError(t, ada.Delete(ctx, settings.DeepCopy()))
	assert.True(t, apierrors.IsNotFound(cl.Get(ctx, key, &corev1.ConfigMap{})))
}

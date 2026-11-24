// RBAC under test. The other suites act as members of system:masters, which
// skips authorization. Here the committed roles in config/rbac are loaded and
// bound to identities that are not: the manager's service account and a
// person who holds the approver role. The design relies on these roles ("the
// manager's rules cannot pass the approval gate"), so a verb that goes missing
// or appears must fail a test, not the lab.
package envtest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/controller"
)

const (
	// managerUser is the username of the deployed manager's service account
	// (config/rbac/service_account.yaml). A client certificate with this
	// name is authorized exactly as the service account's token would be.
	managerUser = "system:serviceaccount:pk-metal:pk-metal-manager"
	// operatorNamespace is the namespace the committed Role names.
	operatorNamespace = "pk-metal"
)

// rbacDocs reads the RBAC objects of one file under config/rbac.
func rbacDocs(t *testing.T, file string) (clusterRoles []rbacv1.ClusterRole, roles []rbacv1.Role, crbs []rbacv1.ClusterRoleBinding, rbs []rbacv1.RoleBinding) {
	t.Helper()
	for _, doc := range strings.Split(readFile(t, filepath.Join("..", "..", "config", "rbac", file)), "\n---") {
		var tm metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal([]byte(doc), &tm))
		switch tm.Kind {
		case "":
			continue
		case "ClusterRole":
			var o rbacv1.ClusterRole
			require.NoError(t, yaml.Unmarshal([]byte(doc), &o))
			clusterRoles = append(clusterRoles, o)
		case "Role":
			var o rbacv1.Role
			require.NoError(t, yaml.Unmarshal([]byte(doc), &o))
			roles = append(roles, o)
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			require.NoError(t, yaml.Unmarshal([]byte(doc), &o))
			crbs = append(crbs, o)
		case "RoleBinding":
			var o rbacv1.RoleBinding
			require.NoError(t, yaml.Unmarshal([]byte(doc), &o))
			rbs = append(rbs, o)
		default:
			t.Fatalf("%s: unexpected kind %s", file, tm.Kind)
		}
	}
	return clusterRoles, roles, crbs, rbs
}

// ensureNamespace creates a namespace unless it exists.
func ensureNamespace(t *testing.T, ctx context.Context, cl client.Client, name string) {
	t.Helper()
	err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
}

// installManagerRBAC applies the generated manager roles and their bindings
// from config/rbac. The namespaced Role and its binding go to namespace
// instead of pk-metal: a suite keeps its Secrets where it likes, and the
// Role's rules are the same wherever it lives. It returns a rest.Config that
// acts as the manager's service account, outside system:masters.
func (s *suite) installManagerRBAC(t *testing.T, ctx context.Context, namespace string) *rest.Config {
	t.Helper()
	ensureNamespace(t, ctx, s.cl, namespace)
	clusterRoles, roles, _, _ := rbacDocs(t, "role.yaml")
	_, _, crbs, rbs := rbacDocs(t, "role_binding.yaml")
	require.Len(t, clusterRoles, 1)
	require.Len(t, roles, 1)
	require.Len(t, crbs, 1)
	require.Len(t, rbs, 1)
	for i := range clusterRoles {
		require.NoError(t, s.cl.Create(ctx, &clusterRoles[i]))
	}
	for i := range crbs {
		require.NoError(t, s.cl.Create(ctx, &crbs[i]))
	}
	for i := range roles {
		roles[i].Namespace = namespace
		require.NoError(t, s.cl.Create(ctx, &roles[i]))
	}
	for i := range rbs {
		rbs[i].Namespace = namespace
		require.NoError(t, s.cl.Create(ctx, &rbs[i]))
	}
	u, err := s.env.AddUser(envtest.User{
		Name:   managerUser,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNamespace},
	}, nil)
	require.NoError(t, err)
	return u.Config()
}

// approverClient is a client acting as a person who holds only the committed
// approver ClusterRole (config/rbac/approver_role.yaml), outside
// system:masters. The role is installed on first use.
func (s *suite) approverClient(t *testing.T, ctx context.Context, name string) client.Client {
	t.Helper()
	clusterRoles, _, _, _ := rbacDocs(t, "approver_role.yaml")
	require.Len(t, clusterRoles, 1)
	role := clusterRoles[0]
	if err := s.cl.Create(ctx, &role); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	require.NoError(t, s.cl.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name + "-" + name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}},
	}))
	u, err := s.env.AddUser(envtest.User{Name: name}, nil)
	require.NoError(t, err)
	c, err := client.New(u.Config(), client.Options{Scheme: s.scheme})
	require.NoError(t, err)
	return c
}

// newManagerAs is a manager that acts with cfg and confines its Secret and
// ConfigMap informers to credentialsNamespace, as cmd/manager does. Without
// that confinement the informers list every namespace, which the namespaced
// Role does not allow.
func (s *suite) newManagerAs(t *testing.T, cfg *rest.Config, credentialsNamespace string) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     s.scheme,
		NewClient:  s.reads.newClient,
		Cache:      cache.Options{ByObject: controller.CacheByObject(credentialsNamespace)},
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	return mgr
}

func TestRBAC(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	managerCfg := st.installManagerRBAC(t, ctx, operatorNamespace)
	managerCl, err := client.New(managerCfg, client.Options{Scheme: st.scheme})
	require.NoError(t, err)
	approverCl := st.approverClient(t, ctx, "alice")
	ensureNamespace(t, ctx, cl, "team-a")

	// allowed asks the authorizer whether user may do verb on the resource.
	allowed := func(user string, groups []string, verb, group, resource, subresource, namespace string) bool {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user, Groups: groups,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: verb, Group: group, Resource: resource, Subresource: subresource, Namespace: namespace,
			},
		}}
		require.NoError(t, cl.Create(ctx, sar))
		return sar.Status.Allowed
	}
	saGroups := []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNamespace, "system:authenticated"}
	personGroups := []string{"system:authenticated"}

	type check struct {
		verb, group, resource, subresource, namespace string
		want                                          bool
	}
	t.Run("the manager proposes and executes but cannot approve", func(t *testing.T) {
		for _, c := range []check{
			// No patch or update on an operation's spec: that is the approval.
			{"patch", "metal.pk.io", "hostoperations", "", "team-a", false},
			{"update", "metal.pk.io", "hostoperations", "", "team-a", false},
			{"create", "metal.pk.io", "hostoperations", "", "team-a", true},
			{"delete", "metal.pk.io", "hostoperations", "", "team-a", true},
			{"update", "metal.pk.io", "hostoperations", "status", "team-a", true},
			// Claims: bound and released, never created by the manager.
			{"update", "metal.pk.io", "hostclaims", "", "team-a", true},
			{"create", "metal.pk.io", "hostclaims", "", "team-a", false},
			// Secrets only in its own namespace.
			{"get", "", "secrets", "", operatorNamespace, true},
			{"list", "", "secrets", "", operatorNamespace, true},
			{"create", "", "secrets", "", operatorNamespace, true},
			{"get", "", "secrets", "", "team-a", false},
			{"list", "", "secrets", "", "", false},
			// The approval policy and its params are a person's to change.
			{"update", "", "configmaps", "", operatorNamespace, false},
			{"create", "admissionregistration.k8s.io", "validatingadmissionpolicies", "", "", false},
			{"create", "rbac.authorization.k8s.io", "clusterrolebindings", "", "", false},
		} {
			name := c.verb + " " + strings.TrimSuffix(c.resource+"/"+c.subresource, "/") + " in " + c.namespace
			assert.Equal(t, c.want, allowed(managerUser, saGroups, c.verb, c.group, c.resource, c.subresource, c.namespace), name)
		}
	})

	t.Run("an approver may approve and nothing more", func(t *testing.T) {
		for _, c := range []check{
			{"patch", "metal.pk.io", "hostoperations", "", "team-a", true},
			{"get", "metal.pk.io", "hostoperations", "", "team-a", true},
			{"update", "metal.pk.io", "hostoperations", "status", "team-a", false},
			{"create", "metal.pk.io", "hostoperations", "", "team-a", false},
			{"delete", "metal.pk.io", "hostoperations", "", "team-a", false},
			{"get", "metal.pk.io", "hosts", "", "", true},
			{"patch", "metal.pk.io", "hosts", "", "", false},
			{"get", "", "secrets", "", operatorNamespace, false},
		} {
			name := c.verb + " " + strings.TrimSuffix(c.resource+"/"+c.subresource, "/") + " in " + c.namespace
			assert.Equal(t, c.want, allowed("alice", personGroups, c.verb, c.group, c.resource, c.subresource, c.namespace), name)
		}
	})

	t.Run("requests as those identities get the same answers", func(t *testing.T) {
		op := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "rbac-probe", Namespace: "team-a"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef: metalv1alpha1.Reference{Name: "no-such-host"}, ProposedBy: managerUser,
				Actions: []metalv1alpha1.HostOperationAction{{PowerOff: &metalv1alpha1.PowerOffAction{}}},
			},
		}
		require.NoError(t, managerCl.Create(ctx, op), "the manager proposes")

		approve := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"`+managerUser+`"}}}`))
		err := managerCl.Patch(ctx, op, approve)
		require.Error(t, err)
		assert.True(t, apierrors.IsForbidden(err), "the manager cannot approve its own proposal: %v", err)

		op.Status.ShutdownRequested = ptr.To(metav1.NowMicro())
		require.NoError(t, managerCl.Status().Update(ctx, op), "the manager reports progress")

		err = approverCl.Status().Update(ctx, op)
		assert.True(t, apierrors.IsForbidden(err), "a person cannot write an operation's progress: %v", err)
		require.NoError(t, approverCl.Patch(ctx, op, client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))), "the approver role is enough to approve")

		sec := &corev1.Secret{}
		err = managerCl.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "anything"}, sec)
		assert.True(t, apierrors.IsForbidden(err), "no Secrets outside the operator's namespace: %v", err)
		err = managerCl.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "anything"}, sec)
		assert.True(t, apierrors.IsNotFound(err), "its own namespace is readable: %v", err)

		require.NoError(t, managerCl.Delete(ctx, op), "the manager withdraws its proposal")
	})
}

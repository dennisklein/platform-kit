package envtest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

const (
	adminGroup = "pk-metal:admins"
	recipientA = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	recipientB = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"
)

// installPolicy loads one committed policy and its binding from config/policy.
// The params are the ones installApprovalPolicy created.
func installPolicy(t *testing.T, ctx context.Context, cl client.Client, file string) {
	t.Helper()
	docs := strings.Split(readFile(t, filepath.Join("..", "..", "config", "policy", file)), "\n---")
	require.Len(t, docs, 2)
	vap := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[0]), vap))
	require.NoError(t, cl.Create(ctx, vap))
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.Unmarshal([]byte(docs[1]), binding))
	require.NoError(t, cl.Create(ctx, binding))
}

// refusedOnceEnforcing waits until a policy compiles and refuses c's create
// of obj with a message that contains want. A policy takes a moment to become
// enforcing after its creation; a create that passes in that moment is
// deleted again, so the next try starts clean.
func refusedOnceEnforcing(t *testing.T, ctx context.Context, admin, c client.Client, obj client.Object, want string) error {
	t.Helper()
	var last error
	require.Eventually(t, func() bool {
		try := obj.DeepCopyObject().(client.Object)
		last = c.Create(ctx, try)
		if last == nil {
			require.NoError(t, admin.Delete(ctx, try))
			return false
		}
		return strings.Contains(last.Error(), want)
	}, 30*time.Second, 100*time.Millisecond, "the policy never refused with %q (last: %v)", want, last)
	return last
}

func TestPolicyStreamC(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	const operator = "op-sa"
	installApprovalPolicy(t, ctx, cl, operator)
	installClaimPolicy(t, ctx, cl)
	installPolicy(t, ctx, cl, "etcdbackuppolicy-guard.yaml")
	installPolicy(t, ctx, cl, "endpointprofile-guard.yaml")

	// userClient adds system:masters: these clients test admission, not RBAC.
	operatorCl := st.userClient(t, operator)
	alice := st.userClient(t, "alice")
	ada := st.userClient(t, "ada", adminGroup)

	t.Run("the operator approves nothing, not even with patch", func(t *testing.T) {
		op := &metalv1alpha1.HostOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "c2-cycle", Namespace: "default"},
			Spec: metalv1alpha1.HostOperationSpec{
				HostRef:    metalv1alpha1.Reference{Name: "h1"},
				ProposedBy: operator,
				Actions:    []metalv1alpha1.HostOperationAction{{PowerCycle: &metalv1alpha1.PowerCycleAction{}}},
			},
		}
		// The first refusal proves the policy enforces.
		probe := op.DeepCopy()
		probe.Name = "c2-probe"
		probe.Spec.Approval.ApprovedBy = operator
		_ = refusedOnceEnforcing(t, ctx, cl, operatorCl, probe, "pre-approved")

		require.NoError(t, operatorCl.Create(ctx, op))
		self := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"`+operator+`"}}}`))
		err := operatorCl.Patch(ctx, op.DeepCopy(), self)
		require.Error(t, err, "the operator must not approve its own proposal")
		assert.Contains(t, err.Error(), "the operator approves nothing")

		byAlice := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"approval":{"approvedBy":"alice"}}}`))
		require.NoError(t, alice.Patch(ctx, op.DeepCopy(), byAlice), "a person approves the operator's proposal")
	})

	t.Run("the claim refusal says where group membership comes from", func(t *testing.T) {
		claim := &metalv1alpha1.HostClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "c4", Namespace: "default"},
			Spec: metalv1alpha1.HostClaimSpec{
				AssetRef: metalv1alpha1.Reference{Name: "a1"},
				OS:       &metalv1alpha1.ClaimOS{NodeRef: metalv1alpha1.Reference{Name: "n1"}},
			},
		}
		err := refusedOnceEnforcing(t, ctx, cl, alice, claim, "only an admin")
		assert.Contains(t, err.Error(), "not from a binding")
		assert.Contains(t, err.Error(), "O= of your client certificate")
		assert.NotContains(t, err.Error(), "ClusterRoleBinding to the group")
	})

	t.Run("only an admin decides who reads the backups and where they go", func(t *testing.T) {
		p := pvcPolicy("c3", "prod", recipientA, 3)
		err := refusedOnceEnforcing(t, ctx, cl, alice, p, "only an admin")
		assert.Contains(t, err.Error(), "encryption or sink")
		require.NoError(t, ada.Create(ctx, p.DeepCopy()))

		mine := client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"encryption":{"ageRecipients":["`+recipientA+`","`+recipientB+`"]}}}`))
		err = alice.Patch(ctx, p.DeepCopy(), mine)
		require.Error(t, err, "a non-admin must not add a recipient")
		assert.Contains(t, err.Error(), "only an admin")

		sink := client.RawPatch(types.MergePatchType,
			[]byte(`{"spec":{"sink":{"pvc":null,"s3":{"endpoint":"https://s3.example","bucket":"mine",`+
				`"credentialsSecretRef":{"name":"mine"}}}}}`))
		err = alice.Patch(ctx, p.DeepCopy(), sink)
		require.Error(t, err, "a non-admin must not move the sink")
		assert.Contains(t, err.Error(), "only an admin")

		interval := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"interval":"2h","suspend":true}}`))
		require.NoError(t, alice.Patch(ctx, p.DeepCopy(), interval), "interval and suspend stay open")
		require.NoError(t, ada.Patch(ctx, p.DeepCopy(), mine), "an admin adds a recipient")
	})

	t.Run("an http S3 endpoint needs allowPlainHTTP", func(t *testing.T) {
		s3 := func(name, endpoint string, plain bool) *osv1alpha1.EtcdBackupPolicy {
			p := pvcPolicy(name, "prod", recipientA, 3)
			p.Spec.Sink = osv1alpha1.EtcdBackupSink{S3: &osv1alpha1.S3Sink{
				Endpoint: endpoint, Bucket: "backups", AllowPlainHTTP: plain,
				CredentialsSecretRef: osv1alpha1.Reference{Name: "s3"},
			}}
			return p
		}
		err := ada.Create(ctx, s3("c3-http", "http://minio.lab:9000", false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "endpoint must be https://")
		require.NoError(t, ada.Create(ctx, s3("c3-http-lab", "http://minio.lab:9000", true)))
		require.NoError(t, ada.Create(ctx, s3("c3-https", "https://s3.example", false)))
	})

	t.Run("only an admin creates or changes an EndpointProfile", func(t *testing.T) {
		ep := &metalv1alpha1.EndpointProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "c10"},
			Spec: metalv1alpha1.EndpointProfileSpec{
				NTP: &metalv1alpha1.NTPPolicy{Servers: []string{"ntp1.example"}},
			},
		}
		err := refusedOnceEnforcing(t, ctx, cl, alice, ep, "only an admin")
		assert.Contains(t, err.Error(), "EndpointProfile")
		require.NoError(t, ada.Create(ctx, ep.DeepCopy()))

		issuer := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"tls":{"issuerRef":{"name":"mine"}}}}`))
		err = alice.Patch(ctx, ep.DeepCopy(), issuer)
		require.Error(t, err, "a non-admin must not name the issuer of the BMC identities")
		assert.Contains(t, err.Error(), "only an admin")
		ntp := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"ntp":{"servers":["ntp.evil.example"]}}}`))
		require.Error(t, alice.Patch(ctx, ep.DeepCopy(), ntp))

		label := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"labels":{"team":"a"}}}`))
		require.NoError(t, alice.Patch(ctx, ep.DeepCopy(), label), "metadata stays open")
		require.NoError(t, ada.Patch(ctx, ep.DeepCopy(), ntp), "an admin changes the profile")
	})

	t.Run("a published credential lives at most 720h", func(t *testing.T) {
		tc := func(name string, ttl time.Duration) *osv1alpha1.TalosCluster {
			return &osv1alpha1.TalosCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: osv1alpha1.TalosClusterSpec{
					TalosVersion: labTalos, KubernetesVersion: "v1.36.2",
					Endpoint:    "https://10.0.0.10:6443",
					SecretsRef:  osv1alpha1.SecretReference{Namespace: "default", Name: name},
					Credentials: &osv1alpha1.CredentialsSpec{TTL: &metav1.Duration{Duration: ttl}},
				},
			}
		}
		err := cl.Create(ctx, tc("c8-long", 721*time.Hour))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ttl must be at most 720h")
		require.NoError(t, cl.Create(ctx, tc("c8-month", 720*time.Hour)))
	})
}

// TestOnCallRole: the committed on-call role covers what the runbooks do,
// and holds neither the approval (patch on operations and rollouts) nor any
// Secret.
func TestOnCallRole(t *testing.T) {
	st := startEnv(t)
	ctx := t.Context()
	cl := st.cl

	clusterRoles, _, _, _ := rbacDocs(t, "oncall_role.yaml")
	require.Len(t, clusterRoles, 1)
	role := clusterRoles[0]
	require.Equal(t, "pk-metal-oncall", role.Name)
	require.NoError(t, cl.Create(ctx, &role))
	require.NoError(t, cl.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pk-metal-oncall-bob"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: "bob"}},
	}))

	allowed := func(verb, group, resource string) bool {
		sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: "bob", Groups: []string{"system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: verb, Group: group, Resource: resource, Namespace: "pk-metal",
			},
		}}
		require.NoError(t, cl.Create(ctx, sar))
		return sar.Status.Allowed
	}
	for _, c := range []struct {
		verb, group, resource string
		want                  bool
		why                   string
	}{
		{"delete", "metal.pk.io", "hostoperations", true, "pk-metal retry"},
		{"patch", "metal.pk.io", "hostclaims", true, "pk-metal hold, unhold"},
		{"patch", "metal.pk.io", "hosts", true, "pk-metal wipe, the disk-not-wiped mark"},
		{"patch", "metal.pk.io", "assets", true, "correct the ledger facts"},
		{"patch", "os.pk.io", "talosclusters", true, "remove restore-pending"},
		{"delete", "os.pk.io", "talosrollouts", true, "stop a rollout for good"},
		{"list", "metal.pk.io", "managementendpoints", true, "describe"},
		{"list", "os.pk.io", "etcdsnapshots", true, "backup-failing"},
		{"list", "", "events", true, "describe"},
		{"patch", "metal.pk.io", "hostoperations", false, "patch is approval"},
		{"patch", "os.pk.io", "talosrollouts", false, "patch is approval"},
		{"create", "metal.pk.io", "hostoperations", false, "proposing is the operator's"},
		{"patch", "metal.pk.io", "endpointprofiles", false, "profiles are an admin's"},
		{"patch", "os.pk.io", "etcdbackuppolicies", false, "backup policies are an admin's"},
		{"get", "", "secrets", false, "no Secret"},
	} {
		assert.Equal(t, c.want, allowed(c.verb, c.group, c.resource), "%s %s/%s (%s)", c.verb, c.group, c.resource, c.why)
	}

	// The manager's /metrics, for a look when Prometheus is down.
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: "bob", Groups: []string{"system:authenticated"},
		NonResourceAttributes: &authorizationv1.NonResourceAttributes{Verb: "get", Path: "/metrics"},
	}}
	require.NoError(t, cl.Create(ctx, sar))
	assert.True(t, sar.Status.Allowed, "get /metrics")
}

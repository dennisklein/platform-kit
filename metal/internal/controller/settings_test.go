package controller

import (
	"bytes"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/metrics"
)

// Each change of the site settings that the operator sees is one audit line
// with the values before and after; a write that leaves them as they are
// writes none.
func TestSettingsChangesAreAudited(t *testing.T) {
	ctx := t.Context()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "pk-metal", Name: siteSettingsConfigMap, UID: "u-settings"},
		Data: map[string]string{"selfApproval": "Forbidden", "adminGroup": "pk-metal:admins",
			"operator": testOperator, "unrelated": "x"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm).Build()
	var buf bytes.Buffer
	r := &SettingsReconciler{Client: cl, Namespace: "pk-metal", Audit: audit.New(&buf, testOperator)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cm)}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, req)
		require.NoError(t, err)
	}

	reconcile()
	reconcile()
	cm.Data["unrelated"] = "y"
	require.NoError(t, cl.Update(ctx, cm))
	reconcile()
	cm.Data["selfApproval"] = "Allowed"
	cm.Data["retention"] = "7d"
	require.NoError(t, cl.Update(ctx, cm))
	reconcile()
	require.NoError(t, cl.Delete(ctx, cm))
	reconcile()

	lines := trail(t, &buf)
	require.Equal(t, []string{"settings.read", "settings.changed", "settings.changed"}, eventNames(lines),
		"a reconcile that sees the same values, or a change of another key, writes no line")
	assert.Equal(t, map[string]string{"selfApproval": "Forbidden", "adminGroup": "pk-metal:admins", "operator": testOperator},
		lines[0].New)
	assert.Equal(t, "Forbidden", lines[1].Old["selfApproval"])
	assert.Equal(t, "Allowed", lines[1].New["selfApproval"])
	assert.Equal(t, "7d", lines[1].New["retention"])
	assert.Empty(t, lines[1].Old["retention"], "a key the ConfigMap did not have is not there")
	assert.Equal(t, "Allowed", lines[2].Old["selfApproval"])
	assert.Empty(t, lines[2].New)
	assert.Contains(t, lines[2].Message, "the ConfigMap is gone")
	for _, l := range lines {
		assert.Equal(t, audit.KindConfigMap, l.Kind)
		assert.Empty(t, l.Actor, "the operator does not know who changed the settings")
	}
}

func gateScheme(t *testing.T) *runtime.Scheme {
	s := testScheme(t)
	require.NoError(t, admissionregistrationv1.AddToScheme(s))
	return s
}

// gate is a policy and its binding as config/policy has them.
func gate(name string, params bool) []client.Object {
	p := &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{FailurePolicy: ptr.To(admissionregistrationv1.Fail)}}
	b := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}}}
	if params {
		p.Spec.ParamKind = &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"}
		b.Spec.ParamRef = &admissionregistrationv1.ParamRef{Name: siteSettingsConfigMap, Namespace: "pk-metal",
			ParameterNotFoundAction: ptr.To(admissionregistrationv1.DenyAction)}
	}
	return []client.Object{p, b}
}

func TestGateProblems(t *testing.T) {
	ctx := t.Context()
	var objs []client.Object
	objs = append(objs, gate("good", true)...)
	objs = append(objs, gate("plain", false)...)
	ignore := gate("ignore", false)
	ignore[0].(*admissionregistrationv1.ValidatingAdmissionPolicy).Spec.FailurePolicy = ptr.To(admissionregistrationv1.Ignore)
	objs = append(objs, ignore...)
	warn := gate("warn", false)
	warn[1].(*admissionregistrationv1.ValidatingAdmissionPolicyBinding).Spec.ValidationActions =
		[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Warn, admissionregistrationv1.Audit}
	objs = append(objs, warn...)
	open := gate("open", true)
	open[1].(*admissionregistrationv1.ValidatingAdmissionPolicyBinding).Spec.ParamRef.ParameterNotFoundAction =
		ptr.To(admissionregistrationv1.AllowAction)
	objs = append(objs, open...)
	objs = append(objs, gate("unbound", false)[0])
	cl := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(objs...).Build()

	got, err := gateProblems(ctx, cl, []string{"good", "plain", "ignore", "warn", "open", "unbound", "missing"})
	require.NoError(t, err)
	assert.NotContains(t, got, "good")
	assert.NotContains(t, got, "plain")
	assert.Contains(t, got["ignore"], "failurePolicy Ignore")
	assert.Contains(t, got["warn"], "validationActions [Deny]")
	assert.Contains(t, got["open"], "parameterNotFoundAction Deny")
	assert.Contains(t, got["unbound"], "has no ValidatingAdmissionPolicyBinding")
	assert.Contains(t, got["missing"], "is missing")
}

// A policy that goes missing is one gate.degraded line and a metric of 1
// until it is back; then one gate.restored line and a metric of 0.
func TestGateCheck(t *testing.T) {
	ctx := t.Context()
	var objs []client.Object
	for _, p := range GatePolicies {
		objs = append(objs, gate(p, false)...)
	}
	cl := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithObjects(objs...).Build()
	var buf bytes.Buffer
	r := &SettingsReconciler{Reader: cl, Audit: audit.New(&buf, testOperator)}
	reg := prometheus.NewRegistry()
	require.NoError(t, metrics.Register(reg, cl, "pk-metal"))
	degraded := func() float64 {
		t.Helper()
		families, err := reg.Gather()
		require.NoError(t, err)
		for _, f := range families {
			if f.GetName() != "pk_metal_admission_gate_degraded" {
				continue
			}
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "policy" && l.GetValue() == "requested-by" {
						return m.GetGauge().GetValue()
					}
				}
			}
		}
		t.Fatal("no series for the policy requested-by")
		return 0
	}

	require.NoError(t, r.checkGate(ctx))
	assert.Zero(t, degraded())
	assert.Empty(t, buf.String(), "a gate in place writes no line")

	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "requested-by"}, binding))
	require.NoError(t, cl.Delete(ctx, binding))
	require.NoError(t, r.checkGate(ctx))
	require.NoError(t, r.checkGate(ctx))
	assert.Equal(t, 1.0, degraded())

	binding.ResourceVersion = ""
	require.NoError(t, cl.Create(ctx, binding))
	require.NoError(t, r.checkGate(ctx))
	assert.Zero(t, degraded())

	lines := trail(t, &buf)
	require.Equal(t, []string{"gate.degraded", "gate.restored"}, eventNames(lines))
	assert.Equal(t, "requested-by", lines[0].Name)
	assert.Contains(t, lines[0].Message, "has no ValidatingAdmissionPolicyBinding")
}

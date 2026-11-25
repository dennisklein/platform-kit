package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/dennisklein/platform-kit/metal/internal/audit"
	"github.com/dennisklein/platform-kit/metal/internal/metrics"
)

// settingsKeys are the keys of the site settings ConfigMap that decide who
// may do what: the audit trail records each change of them.
var settingsKeys = []string{"selfApproval", "retention", "adminGroup", "operator"}

// GatePolicies are the admission policies of the approval gate
// (config/policy). Each must exist with failurePolicy Fail, and have a
// binding that denies; a policy with parameters must deny where its
// parameters are missing. The settings controller checks each of them.
var GatePolicies = []string{
	"hostoperation-approval",
	"talosrollout-approval",
	"hostclaim-provisioning",
	"etcdbackuppolicy-guard",
	"endpointprofile-guard",
	"taloscluster-credentials-guard",
	"guard-lift",
	"requested-by",
	"settings-guard",
}

// defaultGateCheck is how often the settings controller checks the gate.
const defaultGateCheck = time.Minute

// SettingsReconciler watches the site settings ConfigMap
// (hostoperation-approval) and the admission policies that enforce them.
// It writes an audit line for each change of the settings it sees, with the
// values before and after, and checks the gate on a timer: a policy or a
// binding that is missing or does not deny shows in the metric
// pk_metal_admission_gate_degraded and in a gate.degraded audit line. It
// changes nothing: a person must restore the policy.
type SettingsReconciler struct {
	Client client.Client
	// Reader reads the admission policies from the API server: the
	// operator reads them only here, so they are not cached.
	Reader    client.Reader
	Namespace string
	Audit     *audit.Logger
	// Interval paces the gate check; zero is one minute.
	Interval time.Duration

	mu       sync.Mutex
	read     bool
	seen     map[string]string
	degraded map[string]string
}

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings,verbs=get;list

func (r *SettingsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, req.NamespacedName, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.observeSettings(cm, err == nil)
	return ctrl.Result{}, nil
}

// observeSettings compares the settings with what the operator saw last and
// writes the audit line. exists is false when the ConfigMap is gone.
func (r *SettingsReconciler) observeSettings(cm *corev1.ConfigMap, exists bool) {
	values := map[string]string{}
	if exists {
		for _, k := range settingsKeys {
			if v, ok := cm.Data[k]; ok {
				values[k] = v
			}
		}
	}
	o := audit.Object{Kind: audit.KindConfigMap, Namespace: r.Namespace, Name: siteSettingsConfigMap, UID: cm.UID}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.read && !exists:
		r.Audit.SettingsRead(o, nil, settingsGone)
	case !r.read:
		r.Audit.SettingsRead(o, values, "")
	case maps.Equal(r.seen, values) && exists:
		return
	case !exists:
		r.Audit.SettingsChanged(o, r.seen, nil, settingsGone)
	default:
		r.Audit.SettingsChanged(o, r.seen, values, "")
	}
	r.read, r.seen = true, values
}

const settingsGone = "the ConfigMap is gone: every admission policy that reads it refuses each request it checks"

// checkGate checks each gate policy and records the outcome.
func (r *SettingsReconciler) checkGate(ctx context.Context) error {
	problems, err := gateProblems(ctx, r.Reader, GatePolicies)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range GatePolicies {
		problem, bad := problems[p]
		metrics.SetGateDegraded(p, bad)
		before, was := r.degraded[p]
		o := audit.Object{Kind: audit.KindAdmissionPolicy, Name: p}
		switch {
		case bad && (!was || before != problem):
			r.Audit.GateDegraded(o, problem)
		case !bad && was:
			r.Audit.GateRestored(o)
		}
	}
	r.degraded = problems
	return nil
}

// gateProblems says, for each named policy that does not hold the gate,
// what is wrong with it.
func gateProblems(ctx context.Context, c client.Reader, policies []string) (map[string]string, error) {
	bindings := &admissionregistrationv1.ValidatingAdmissionPolicyBindingList{}
	if err := c.List(ctx, bindings); err != nil {
		return nil, fmt.Errorf("list admission policy bindings: %w", err)
	}
	problems := map[string]string{}
	for _, name := range policies {
		p := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, p); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("read admission policy %s: %w", name, err)
			}
			problems[name] = fmt.Sprintf("the ValidatingAdmissionPolicy %s is missing: apply metal/config/policy", name)
			continue
		}
		if fp := p.Spec.FailurePolicy; fp != nil && *fp != admissionregistrationv1.Fail {
			problems[name] = fmt.Sprintf("the policy %s has failurePolicy %s: a request it cannot check passes; set Fail", name, *fp)
			continue
		}
		if why := bindingProblem(p, bindings.Items); why != "" {
			problems[name] = why
		}
	}
	return problems, nil
}

// bindingProblem says why no binding of p denies, or "" when one does.
func bindingProblem(p *admissionregistrationv1.ValidatingAdmissionPolicy,
	bindings []admissionregistrationv1.ValidatingAdmissionPolicyBinding) string {
	var found []string
	for i := range bindings {
		b := &bindings[i]
		if b.Spec.PolicyName != p.Name {
			continue
		}
		found = append(found, b.Name)
		if !slices.Contains(b.Spec.ValidationActions, admissionregistrationv1.Deny) {
			continue
		}
		if p.Spec.ParamKind != nil {
			ref := b.Spec.ParamRef
			if ref == nil || ref.ParameterNotFoundAction == nil || *ref.ParameterNotFoundAction != admissionregistrationv1.DenyAction {
				continue
			}
		}
		return ""
	}
	if len(found) == 0 {
		return fmt.Sprintf("the policy %s has no ValidatingAdmissionPolicyBinding, so it checks nothing: apply metal/config/policy", p.Name)
	}
	need := "validationActions [Deny]"
	if p.Spec.ParamKind != nil {
		need += " and parameterNotFoundAction Deny"
	}
	return fmt.Sprintf("no binding of the policy %s (%s) denies: each needs %s", p.Name, strings.Join(found, ", "), need)
}

// Start runs the gate check until ctx ends. It runs on the leader only.
func (r *SettingsReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultGateCheck
	}
	log := logf.FromContext(ctx).WithName("settings")
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.checkGate(ctx); err != nil {
			log.Error(err, "check the admission gate")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *SettingsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(manager.RunnableFunc(r.Start)); err != nil {
		return err
	}
	only := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == r.Namespace && o.GetName() == siteSettingsConfigMap
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("settings").
		For(&corev1.ConfigMap{}, builder.WithPredicates(only)).
		Complete(r)
}

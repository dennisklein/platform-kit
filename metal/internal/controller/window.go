package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// Maintenance windows (MaintenanceWindow, metal.pk.io) gate when approved
// work may START on the machines they cover. The executor asks once, before
// an operation's baseline is written (holdForWindow); an operation that has
// started is never asked again, so a window closing never stops a step half
// way. The window math is pure (windowsOpen) and the rest only joins it with
// the cluster state.

// windowRecheck caps how long a held operation sleeps before it looks again.
// The executor does not watch the windows themselves, so an edit (a new
// window, an annotation to skip them) is seen within this time.
const windowRecheck = 5 * time.Minute

// windowNow is the clock of the window gate; tests replace it.
var windowNow = time.Now

var weekdays = map[metalv1alpha1.Weekday]time.Weekday{
	"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday,
	"Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday,
}

// maxWeeklyDuration bounds a weekly window: a longer one would overlap its
// own next week, which says "always" in a confusing way.
const maxWeeklyDuration = 7 * 24 * time.Hour

// parseWindow checks what the schema cannot: the time zone exists, and the
// weekly durations are in range. A window that does not parse is never open.
func parseWindow(w *metalv1alpha1.MaintenanceWindow) (*time.Location, error) {
	tz := w.Spec.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("time zone %q: %w", tz, err)
	}
	for i, wk := range w.Spec.Weekly {
		if d := wk.Duration.Duration; d < time.Minute || d > maxWeeklyDuration {
			return nil, fmt.Errorf("weekly[%d]: duration %s is outside 1m..168h", i, d)
		}
		if _, err := time.Parse("15:04", wk.Start); err != nil {
			return nil, fmt.Errorf("weekly[%d]: start %q is not HH:MM", i, wk.Start)
		}
		for _, d := range wk.Days {
			if _, ok := weekdays[d]; !ok {
				return nil, fmt.Errorf("weekly[%d]: unknown day %q", i, d)
			}
		}
	}
	if w.Spec.HostSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(w.Spec.HostSelector); err != nil {
			return nil, fmt.Errorf("hostSelector: %w", err)
		}
	}
	return loc, nil
}

// windowState says whether w is open at now, and when that next changes
// (zero when it never does again). The weekly slots are laid out in the
// window's own time zone, day by day, so a slot keeps its local start time
// across a daylight-saving change.
func windowState(w *metalv1alpha1.MaintenanceWindow, loc *time.Location, now time.Time) (open bool, next time.Time) {
	type span struct{ start, end time.Time }
	var spans []span
	for _, o := range w.Spec.Once {
		spans = append(spans, span{o.Start.Time, o.End.Time})
	}
	local := now.In(loc)
	// A slot can last up to a week, so one that started up to eight days ago
	// may still be open; the next start is at most a week ahead.
	for day := -8; day <= 8; day++ {
		d := time.Date(local.Year(), local.Month(), local.Day()+day, 0, 0, 0, 0, loc)
		for _, wk := range w.Spec.Weekly {
			if !slices.ContainsFunc(wk.Days, func(x metalv1alpha1.Weekday) bool { return weekdays[x] == d.Weekday() }) {
				continue
			}
			hm, _ := time.Parse("15:04", wk.Start)
			start := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
			spans = append(spans, span{start, start.Add(wk.Duration.Duration)})
		}
	}
	// Open now: the change is when the last of the overlapping open spans
	// ends (spans chained back to back keep it open). Closed: the earliest
	// start ahead.
	closeAt := time.Time{}
	for _, s := range spans {
		if !now.Before(s.start) && now.Before(s.end) && s.end.After(closeAt) {
			closeAt = s.end
		}
	}
	if !closeAt.IsZero() {
		for extended := true; extended; {
			extended = false
			for _, s := range spans {
				if !closeAt.Before(s.start) && closeAt.Before(s.end) {
					closeAt, extended = s.end, true
				}
			}
		}
		return true, closeAt
	}
	for _, s := range spans {
		if s.start.After(now) && (next.IsZero() || s.start.Before(next)) {
			next = s.start
		}
	}
	return false, next
}

// windowCovers reports whether w covers the host: its labels match the
// selector, or the node it runs belongs to a named cluster.
func windowCovers(w *metalv1alpha1.MaintenanceWindow, host *metalv1alpha1.Host, cluster string) bool {
	if sel := w.Spec.HostSelector; sel != nil {
		if ok, err := selectorMatches(*sel, host.Labels); err == nil && ok {
			return true
		}
	}
	return cluster != "" && slices.Contains(w.Spec.Clusters, cluster)
}

// windowExempt reports why an operation is not held by any window, or "".
func windowExempt(op *metalv1alpha1.HostOperation) string {
	if why, ok := op.Annotations[metalv1alpha1.AnnotationIgnoreWindow]; ok {
		return "annotated " + metalv1alpha1.AnnotationIgnoreWindow + ": " + why
	}
	if op.Spec.Reason == reasonProvisioning {
		return "a new install disturbs no running workload"
	}
	return ""
}

// windowVerdict is what the gate decided for one operation.
type windowVerdict struct {
	// held is the message when the operation must wait; "" lets it start.
	held string
	// until is when to look again.
	until time.Time
}

// decideWindow is the gate's pure part: given the windows that exist, may
// an operation on this host start at now?
func decideWindow(windows []metalv1alpha1.MaintenanceWindow, op *metalv1alpha1.HostOperation,
	host *metalv1alpha1.Host, cluster string, now time.Time) windowVerdict {
	if windowExempt(op) != "" {
		return windowVerdict{}
	}
	var covering, broken []string
	var next time.Time
	for i := range windows {
		w := &windows[i]
		if !w.DeletionTimestamp.IsZero() || !windowCovers(w, host, cluster) {
			continue
		}
		covering = append(covering, w.Name)
		loc, err := parseWindow(w)
		if err != nil {
			// Fail closed: the person meant to restrict these machines.
			broken = append(broken, fmt.Sprintf("%s (%v)", w.Name, err))
			continue
		}
		open, change := windowState(w, loc, now)
		if open {
			return windowVerdict{}
		}
		if !change.IsZero() && (next.IsZero() || change.Before(next)) {
			next = change
		}
	}
	if len(covering) == 0 {
		return windowVerdict{}
	}
	msg := fmt.Sprintf("host %s is covered by maintenance window %s, and none is open", host.Name, strings.Join(covering, ", "))
	switch {
	case !next.IsZero():
		msg += fmt.Sprintf("; the next opens at %s (in %s), and this operation starts then",
			next.UTC().Format(time.RFC3339), next.Sub(now).Round(time.Minute))
	default:
		msg += "; none of them opens again"
	}
	if len(broken) > 0 {
		msg += "; cannot read " + strings.Join(broken, ", ")
	}
	msg += fmt.Sprintf(". To start it now: kubectl annotate hostoperation -n %s %s %s=\"<reason>\"",
		op.Namespace, op.Name, metalv1alpha1.AnnotationIgnoreWindow)
	until := now.Add(windowRecheck)
	if !next.IsZero() && next.Before(until) {
		until = next
	}
	return windowVerdict{held: msg, until: until}
}

// hostCluster is the TalosCluster the host's node belongs to, or "" when it
// runs no node the operator knows.
func hostCluster(ctx context.Context, c client.Reader, host *metalv1alpha1.Host) (string, error) {
	if host.Spec.OS == nil {
		return "", nil
	}
	node := &osv1alpha1.TalosNode{}
	if err := c.Get(ctx, client.ObjectKey{Name: host.Spec.OS.NodeRef.Name}, node); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return node.Spec.ClusterRef.Name, nil
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=maintenancewindows,verbs=get;list;watch

// holdForWindow is the executor's window gate, asked before an operation
// starts. held reports that the operation waits: its Executed condition
// says why (OutsideMaintenanceWindow, which becomes its blockedOn), and the
// result brings it back when the next window opens.
func (r *HostOperationReconciler) holdForWindow(ctx context.Context, op *metalv1alpha1.HostOperation, host *metalv1alpha1.Host,
	setCond func(string, metav1.ConditionStatus, string, string), flush func() error) (ctrl.Result, bool, error) {
	if windowExempt(op) != "" {
		return ctrl.Result{}, false, nil
	}
	windows := &metalv1alpha1.MaintenanceWindowList{}
	if err := r.List(ctx, windows); err != nil {
		return ctrl.Result{}, true, err
	}
	if len(windows.Items) == 0 {
		return ctrl.Result{}, false, nil
	}
	cluster, err := hostCluster(ctx, r.Client, host)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	now := windowNow()
	v := decideWindow(windows.Items, op, host, cluster, now)
	if v.held == "" {
		return ctrl.Result{}, false, nil
	}
	setCond(metalv1alpha1.ConditionExecuted, metav1.ConditionUnknown, metalv1alpha1.ReasonOutsideMaintenanceWindow, v.held)
	// A second past the opening, so the pass lands inside the window.
	return ctrl.Result{RequeueAfter: v.until.Sub(now) + time.Second}, true, client.IgnoreNotFound(flush())
}

// MaintenanceWindowReconciler keeps a window's status current: whether it is
// open and when that changes, for people and kubectl columns. The executor
// does not read this status; it works the windows out itself, so a status a
// pass behind can never let work start.
type MaintenanceWindowReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=metal.pk.io,resources=maintenancewindows/status,verbs=update

func (r *MaintenanceWindowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	w := &metalv1alpha1.MaintenanceWindow{}
	if err := r.Get(ctx, req.NamespacedName, w); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := w.Status.DeepCopy()
	now := windowNow()
	loc, err := parseWindow(w)
	var next time.Time
	if err != nil {
		w.Status.Open, w.Status.NextChange = false, nil
		setCondition(&w.Status.Conditions, w.Generation, metalv1alpha1.ConditionValid, metav1.ConditionFalse,
			metalv1alpha1.ReasonWindowInvalid, err.Error()+"; the window is never open, so the machines it covers are held")
	} else {
		w.Status.Open, next = windowState(w, loc, now)
		w.Status.NextChange = nil
		if !next.IsZero() {
			w.Status.NextChange = &metav1.Time{Time: next.Truncate(time.Second)}
		}
		setCondition(&w.Status.Conditions, w.Generation, metalv1alpha1.ConditionValid, metav1.ConditionTrue,
			metalv1alpha1.ReasonWindowValid, "")
	}
	if !equality.Semantic.DeepEqual(orig, &w.Status) {
		if err := r.Status().Update(ctx, w); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if next.IsZero() {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: next.Sub(now) + time.Second}, nil
}

func (r *MaintenanceWindowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.MaintenanceWindow{}).
		Named("maintenancewindow").
		Complete(r)
}

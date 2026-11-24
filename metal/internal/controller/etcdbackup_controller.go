package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/etcdbackup"
	"github.com/dennisklein/platform-kit/metal/internal/talos"
	"github.com/dennisklein/platform-kit/metal/internal/talos/render"
)

// etcd backups. Two reconcilers share one lane:
//
//   - EtcdBackupPolicyReconciler schedules: it creates an EtcdSnapshot when
//     the interval has passed, prunes by retention (the stored object first,
//     then its record) and sums the snapshots up in the policy's status.
//   - EtcdSnapshotReconciler takes each new EtcdSnapshot: it asks a healthy
//     control plane (a follower when there is one, so the leader is not
//     slowed) for a snapshot and streams it through verification and age
//     encryption into the sink (internal/etcdbackup).
//
// A snapshot is a read: it changes no machine, so it needs no approval, the
// same as the check-in. The operator holds only public keys, so it can write
// a backup but never read one; a restore needs a person (pk-metal restore).

// Defaults of the backup lane.
const (
	// snapshotTimeout bounds one snapshot stream. etcd's default quota is
	// 2 GiB and its maximum 8 GiB; at the tens of MB/s a sink takes, half
	// an hour covers the maximum.
	snapshotTimeout = 30 * time.Minute
	// memberWait is how long a snapshot waits for a healthy etcd member
	// before it is recorded as failed.
	memberWait = 10 * time.Minute
	// keepFailedSnapshots failures are kept for a person to read why.
	keepFailedSnapshots = 3
	// defaultBackupVolumeName names the backup volume in pvc:// URLs.
	defaultBackupVolumeName = "pk-metal-etcd-backups"
)

// EtcdBackups is what both backup reconcilers need.
type EtcdBackups struct {
	client.Client
	// Namespace is the operator's: the secrets bundles and the S3
	// credentials Secrets are read there.
	Namespace string
	// Dial reaches the control planes' Talos API.
	Dial talos.Dialer
	// Dir is where the manager's backup volume is mounted; empty when none
	// is, which leaves pvc sinks unavailable. VolumeName names it in URLs.
	Dir        string
	VolumeName string
	// Self locates the cluster the operator runs on, which a pvc sink may
	// not back up. Nil (no node name) refuses no cluster.
	Self *SelfGuard
	// RetryInterval paces a snapshot that waits for a healthy member, and
	// the re-check of a policy that cannot run; zero is 30s.
	RetryInterval time.Duration
	// Now defaults to time.Now; tests move it.
	Now func() time.Time
}

func (b *EtcdBackups) retry() time.Duration { return cmp.Or(b.RetryInterval, 30*time.Second) }

func (b *EtcdBackups) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// sinkError is a sink that cannot be used, with the Ready reason to show.
type sinkError struct{ reason, msg string }

func (e *sinkError) Error() string { return e.msg }

// ownCluster names the TalosCluster the operator's pod runs on, "" when
// none of them (kind, a cluster built otherwise).
func (b *EtcdBackups) ownCluster(ctx context.Context) (string, error) {
	if b.Self == nil || b.Self.NodeName == "" {
		return "", nil
	}
	var reader client.Reader = b.Client
	if b.Self.Reader != nil {
		reader = b.Self.Reader
	}
	var k8sNode *corev1.Node
	n := &corev1.Node{}
	if err := reader.Get(ctx, types.NamespacedName{Name: b.Self.NodeName}, n); err == nil {
		k8sNode = n
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	nodes := &osv1alpha1.TalosNodeList{}
	if err := reader.List(ctx, nodes); err != nil {
		return "", err
	}
	if loc := locateSelf(b.Self.NodeName, k8sNode, nodes.Items); loc.node != nil {
		return loc.node.Spec.ClusterRef.Name, nil
	}
	return "", nil
}

// sink builds the policy's sink and the key prefix of its cluster. A
// *sinkError says why it cannot be used.
func (b *EtcdBackups) sink(ctx context.Context, p *osv1alpha1.EtcdBackupPolicy) (etcdbackup.Sink, string, error) {
	cluster := p.Spec.ClusterRef.Name
	switch s := p.Spec.Sink; {
	case s.PVC != nil:
		own, err := b.ownCluster(ctx)
		if err != nil {
			return nil, "", err
		}
		if own == cluster {
			return nil, "", &sinkError{osv1alpha1.ReasonSinkRefused, fmt.Sprintf(
				"the operator runs on cluster %s, so its backup volume lives on the etcd it would back up and is "+
					"lost with it: use an s3 sink for this cluster", cluster)}
		}
		if b.Dir == "" {
			return nil, "", &sinkError{osv1alpha1.ReasonSinkUnavailable,
				"no backup volume is mounted into the manager: add the config/etcd-backup component " +
					"(it sets --etcd-backup-dir), or use an s3 sink"}
		}
		return etcdbackup.FileSink{Root: b.Dir, Name: cmp.Or(b.VolumeName, defaultBackupVolumeName)}, cluster + "/", nil
	case s.S3 != nil:
		creds, err := b.secretData(ctx, s.S3.CredentialsSecretRef.Name)
		if err != nil {
			return nil, "", err
		}
		cfg := etcdbackup.S3Config{
			Endpoint: strings.TrimSuffix(s.S3.Endpoint, "/"), Bucket: s.S3.Bucket, Region: s.S3.Region,
			PathStyle: s.S3.PathStyle, AccessKeyID: string(creds["accessKeyID"]),
			SecretAccessKey: string(creds["secretAccessKey"]),
		}
		if s.S3.CASecretRef != nil {
			ca, err := b.secretData(ctx, s.S3.CASecretRef.Name)
			if err != nil {
				return nil, "", err
			}
			cfg.CA = ca["ca.crt"]
		}
		sink, err := etcdbackup.NewS3Sink(cfg)
		if err != nil {
			return nil, "", &sinkError{osv1alpha1.ReasonSinkUnavailable, err.Error()}
		}
		return sink, s.S3.Prefix + cluster + "/", nil
	}
	return nil, "", &sinkError{osv1alpha1.ReasonSinkUnavailable, "the policy names no sink"}
}

func (b *EtcdBackups) secretData(ctx context.Context, name string) (map[string][]byte, error) {
	s := &corev1.Secret{}
	if err := b.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &sinkError{osv1alpha1.ReasonSinkUnavailable,
				fmt.Sprintf("Secret %s/%s not found (sink Secrets live in the operator's namespace)", b.Namespace, name)}
		}
		return nil, err
	}
	return s.Data, nil
}

func recipients(p *osv1alpha1.EtcdBackupPolicy) ([]age.Recipient, error) {
	rs, err := etcdbackup.ParseRecipients(p.Spec.Encryption.AgeRecipients)
	if err != nil {
		return nil, &sinkError{osv1alpha1.ReasonRecipientsInvalid, err.Error()}
	}
	return rs, nil
}

// snapshotState reads a snapshot record for retention and summaries.
func snapshotState(s *osv1alpha1.EtcdSnapshot) etcdbackup.State {
	c := meta.FindStatusCondition(s.Status.Conditions, osv1alpha1.ConditionComplete)
	switch {
	case c == nil || c.Status == metav1.ConditionUnknown:
		return etcdbackup.InProgress
	case c.Status == metav1.ConditionTrue &&
		meta.IsStatusConditionTrue(s.Status.Conditions, osv1alpha1.ConditionVerified):
		return etcdbackup.Verified
	}
	return etcdbackup.Failed
}

// snapshotTime is when a snapshot was taken, or created when it never was.
func snapshotTime(s *osv1alpha1.EtcdSnapshot) time.Time {
	if s.Status.TakenAt != nil {
		return s.Status.TakenAt.Time
	}
	return s.CreationTimestamp.Time
}

// +kubebuilder:rbac:groups=os.pk.io,resources=etcdbackuppolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=os.pk.io,resources=etcdbackuppolicies/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=etcdsnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=os.pk.io,resources=etcdsnapshots/status,verbs=update
// +kubebuilder:rbac:groups=os.pk.io,resources=talosclusters;talosnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=pk-metal

// EtcdBackupPolicyReconciler schedules, prunes and summarizes one policy.
type EtcdBackupPolicyReconciler struct {
	*EtcdBackups
}

func (r *EtcdBackupPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p := &osv1alpha1.EtcdBackupPolicy{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := p.DeepCopy()
	res, err := r.reconcile(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := updateStatusIfChanged(ctx, r.Client, p, &orig.Status, &p.Status); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *EtcdBackupPolicyReconciler) reconcile(ctx context.Context, p *osv1alpha1.EtcdBackupPolicy) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()
	interval := p.Spec.Interval.Duration
	set := func(t string, s metav1.ConditionStatus, reason, msg string) {
		setCondition(&p.Status.Conditions, p.Generation, t, s, reason, msg)
	}

	snaps := &osv1alpha1.EtcdSnapshotList{}
	if err := r.List(ctx, snaps, client.MatchingLabels{osv1alpha1.LabelSnapshotPolicy: p.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// Ready: can snapshots be taken at all?
	ready := true
	sink, _, err := r.sink(ctx, p)
	if err == nil {
		_, err = recipients(p)
	}
	if err == nil {
		cluster := &osv1alpha1.TalosCluster{}
		if gerr := r.Get(ctx, types.NamespacedName{Name: p.Spec.ClusterRef.Name}, cluster); apierrors.IsNotFound(gerr) {
			err = &sinkError{osv1alpha1.ReasonClusterNotFound, "TalosCluster " + p.Spec.ClusterRef.Name + " not found"}
		} else if gerr != nil {
			return ctrl.Result{}, gerr
		}
	}
	if se := (*sinkError)(nil); errors.As(err, &se) {
		ready = false
		set(osv1alpha1.ConditionReady, metav1.ConditionFalse, se.reason, se.msg)
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		set(osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonSinkReady, "snapshots go to "+sink.URL(""))
	}

	// Retention. A stored object is deleted before its record, so that a
	// failure leaves a record pointing at something, never an object
	// nothing points at. Without a usable sink, stored snapshots stay.
	var recs []etcdbackup.Record
	byName := map[string]*osv1alpha1.EtcdSnapshot{}
	for i := range snaps.Items {
		s := &snaps.Items[i]
		byName[s.Name] = s
		recs = append(recs, etcdbackup.Record{Name: s.Name, Time: snapshotTime(s), State: snapshotState(s)})
	}
	ret := etcdbackup.Retention{Count: int(cmp.Or(p.Spec.Retention.Count, 14)), KeepFailed: keepFailedSnapshots}
	if p.Spec.Retention.MaxAge != nil {
		ret.MaxAge = p.Spec.Retention.MaxAge.Duration
	}
	pruned := map[string]bool{}
	for _, name := range etcdbackup.Prune(recs, ret, now) {
		s := byName[name]
		if s.Status.Key != "" {
			if sink == nil {
				continue
			}
			if err := sink.Delete(ctx, s.Status.Key); err != nil {
				log.Error(err, "prune: delete stored snapshot", "snapshot", name)
				continue
			}
		}
		if err := r.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		pruned[name] = true
		log.Info("pruned etcd snapshot", "policy", p.Name, "snapshot", name, "location", s.Status.Location)
	}

	// The summary, newest first.
	kept := slices.DeleteFunc(slices.Clone(snaps.Items), func(s osv1alpha1.EtcdSnapshot) bool { return pruned[s.Name] })
	slices.SortFunc(kept, func(a, b osv1alpha1.EtcdSnapshot) int {
		return cmp.Or(snapshotTime(&b).Compare(snapshotTime(&a)), strings.Compare(b.Name, a.Name))
	})
	st := &p.Status
	st.LastSuccessfulSnapshot, st.ConsecutiveFailures, st.VerifiedSnapshots = nil, 0, 0
	inProgress := false
	lastFailure := ""
	for i := range kept {
		s := &kept[i]
		switch snapshotState(s) {
		case etcdbackup.InProgress:
			inProgress = true
		case etcdbackup.Verified:
			st.VerifiedSnapshots++
			if st.LastSuccessfulSnapshot == nil {
				st.LastSuccessfulSnapshot = &osv1alpha1.SnapshotReference{Name: s.Name, TakenAt: metav1.NewTime(snapshotTime(s))}
			}
		case etcdbackup.Failed:
			if st.LastSuccessfulSnapshot == nil {
				st.ConsecutiveFailures++
				if lastFailure == "" {
					lastFailure = failureOf(s)
				}
			}
		}
	}
	window := 2 * interval
	var flip time.Duration // until BackedUp turns False on its own
	switch last := st.LastSuccessfulSnapshot; {
	case last == nil:
		msg := "no verified snapshot yet"
		if lastFailure != "" {
			msg += fmt.Sprintf("; %d failed, the newest: %s", st.ConsecutiveFailures, lastFailure)
		}
		set(osv1alpha1.ConditionBackedUp, metav1.ConditionFalse, osv1alpha1.ReasonNoSnapshot, msg)
	case now.Sub(last.TakenAt.Time) > window:
		msg := fmt.Sprintf("the newest verified snapshot, %s, was taken at %s, more than twice the interval (%s) ago",
			last.Name, last.TakenAt.UTC().Format(time.RFC3339), interval)
		if lastFailure != "" {
			msg += fmt.Sprintf("; %d failed since, the newest: %s", st.ConsecutiveFailures, lastFailure)
		}
		set(osv1alpha1.ConditionBackedUp, metav1.ConditionFalse, osv1alpha1.ReasonBackupTooOld, msg)
	default:
		set(osv1alpha1.ConditionBackedUp, metav1.ConditionTrue, osv1alpha1.ReasonSnapshotRecent,
			fmt.Sprintf("%s taken at %s", last.Name, last.TakenAt.UTC().Format(time.RFC3339)))
		flip = last.TakenAt.Add(window).Sub(now) + time.Second
	}

	// The schedule. One snapshot of a policy at a time: a slow sink must
	// not pile up streams from the same etcd.
	next := now
	if st.LastScheduleTime != nil {
		next = st.LastScheduleTime.Add(interval)
	}
	if !p.Spec.Suspend && ready && !inProgress && !now.Before(next) {
		name := snapshotName(p.Name, now)
		snap := &osv1alpha1.EtcdSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				osv1alpha1.LabelSnapshotPolicy:  p.Name,
				osv1alpha1.LabelSnapshotCluster: p.Spec.ClusterRef.Name,
			}},
			Spec: osv1alpha1.EtcdSnapshotSpec{PolicyRef: osv1alpha1.Reference{Name: p.Name}},
		}
		if err := r.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		st.LastScheduleTime = &metav1.Time{Time: now}
		next = now.Add(interval)
		log.Info("scheduled etcd snapshot", "policy", p.Name, "snapshot", name)
	}

	wait := next.Sub(now)
	if p.Spec.Suspend || !ready || inProgress {
		// Re-check soon: a Secret or volume may appear, and a snapshot
		// that ends wakes this reconciler through the watch anyway.
		wait = min(cmp.Or(wait, r.retry()), r.retry())
	}
	if flip > 0 {
		wait = min(wait, flip)
	}
	return ctrl.Result{RequeueAfter: max(wait, min(time.Second, r.retry()))}, nil
}

// snapshotName is <policy>-<UTC time>, which sorts by time and says when.
func snapshotName(policy string, at time.Time) string {
	return fmt.Sprintf("%s-%s", policy, at.UTC().Format("20060102-150405"))
}

func failureOf(s *osv1alpha1.EtcdSnapshot) string {
	for _, t := range []string{osv1alpha1.ConditionComplete, osv1alpha1.ConditionVerified} {
		if c := meta.FindStatusCondition(s.Status.Conditions, t); c != nil && c.Status == metav1.ConditionFalse {
			return s.Name + ": " + c.Message
		}
	}
	return s.Name
}

func (r *EtcdBackupPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Not on its own status writes; on spec and annotation changes, so
		// that a touched policy is looked at again at once.
		For(&osv1alpha1.EtcdBackupPolicy{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&osv1alpha1.EtcdSnapshot{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []ctrl.Request {
				if p := obj.GetLabels()[osv1alpha1.LabelSnapshotPolicy]; p != "" {
					return []ctrl.Request{requestFor(p)}
				}
				return nil
			})).
		Named("etcdbackuppolicy").
		Complete(r)
}

// EtcdSnapshotReconciler takes each new snapshot, once.
type EtcdSnapshotReconciler struct {
	*EtcdBackups
}

func (r *EtcdSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &osv1alpha1.EtcdSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !s.DeletionTimestamp.IsZero() || snapshotState(s) != etcdbackup.InProgress {
		return ctrl.Result{}, nil
	}
	orig := s.DeepCopy()
	res, err := r.take(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := updateStatusIfChanged(ctx, r.Client, s, &orig.Status, &s.Status); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

// take runs one snapshot to its end, or reports what it waits for.
func (r *EtcdSnapshotReconciler) take(ctx context.Context, s *osv1alpha1.EtcdSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	st := &s.Status
	set := func(t string, cs metav1.ConditionStatus, reason, msg string) {
		setCondition(&st.Conditions, s.Generation, t, cs, reason, msg)
	}
	fail := func(msg string) (ctrl.Result, error) {
		set(osv1alpha1.ConditionComplete, metav1.ConditionFalse, osv1alpha1.ReasonSnapshotFailed, msg)
		log.Info("etcd snapshot failed", "snapshot", s.Name, "reason", msg)
		return ctrl.Result{}, nil
	}

	if st.TakenAt != nil {
		// A stream was started and its end never recorded: this process
		// (or another) stopped in the middle. It cannot be resumed; the
		// partial object, if any, goes, and the next schedule tries again.
		if p, err := r.policy(ctx, s); err == nil {
			if sink, _, err := r.sink(ctx, p); err == nil && st.Key != "" {
				_ = sink.Delete(ctx, st.Key)
			}
		}
		return fail("interrupted: the operator stopped while the snapshot streamed")
	}

	p, err := r.policy(ctx, s)
	if apierrors.IsNotFound(err) {
		return fail("EtcdBackupPolicy " + s.Spec.PolicyRef.Name + " not found")
	} else if err != nil {
		return ctrl.Result{}, err
	}
	st.ClusterName = p.Spec.ClusterRef.Name
	sink, prefix, err := r.sink(ctx, p)
	if se := (*sinkError)(nil); errors.As(err, &se) {
		return fail(se.msg)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	rcpt, err := recipients(p)
	if err != nil {
		return fail(err.Error())
	}

	cluster := &osv1alpha1.TalosCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.ClusterRef.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("TalosCluster " + p.Spec.ClusterRef.Name + " not found")
		}
		return ctrl.Result{}, err
	}
	bundle, err := loadBundle(ctx, r.Client, r.Namespace, cluster)
	if unavailable := (*bundleUnavailable)(nil); errors.As(err, &unavailable) {
		return fail(unavailable.msg)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	nodes, err := clusterMembers(ctx, r.Client, cluster.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	var cps []osv1alpha1.TalosNode
	var endpoints []string
	for _, n := range nodes {
		if addr := render.NodeAddress(&n); n.Spec.Role == osv1alpha1.RoleControlPlane && addr != "" {
			cps = append(cps, n)
			endpoints = append(endpoints, addr)
		}
	}
	talosconfig, err := render.Talosconfig(bundle, cluster.Name, endpoints, checkinCertTTL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mint talosconfig for %s: %w", cluster.Name, err)
	}

	src, why := r.pickMember(ctx, talosconfig, cps)
	if src == nil {
		if r.now().Sub(s.CreationTimestamp.Time) > memberWait {
			return fail("no healthy etcd member to take the snapshot from: " + why)
		}
		set(osv1alpha1.ConditionComplete, metav1.ConditionUnknown, osv1alpha1.ReasonTaking,
			"waiting for a healthy etcd member: "+why)
		return ctrl.Result{RequeueAfter: r.retry()}, nil
	}

	// Record the start before streaming: the crash rail above reads it.
	key := prefix + s.Name + ".db.age"
	st.TakenAt = &metav1.Time{Time: r.now()}
	st.TakenFrom, st.MemberID = src.node, strconv.FormatUint(src.status.MemberID, 16)
	st.RaftIndex = int64(min(src.status.RaftIndex, uint64(1<<63-1)))
	st.Key, st.Location = key, sink.URL(key)
	st.Recipients = slices.Clone(p.Spec.Encryption.AgeRecipients)
	set(osv1alpha1.ConditionComplete, metav1.ConditionUnknown, osv1alpha1.ReasonTaking,
		"streaming from "+src.node+" to "+st.Location)
	if err := r.Status().Update(ctx, s); err != nil {
		return ctrl.Result{}, err
	}

	sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	var stored etcdbackup.Stored
	err = talos.With(sctx, r.Dial, talos.Target{Endpoint: src.endpoint, Talosconfig: talosconfig}, func(c talos.Client) error {
		var serr error
		stored, serr = etcdbackup.Store(sctx, sink, key, rcpt, c.EtcdSnapshot)
		return serr
	})
	if err != nil {
		return fail(fmt.Sprintf("from %s to %s: %v", src.node, st.Location, err))
	}
	st.Size, st.SHA256 = stored.Plain.Size, "sha256:"+stored.Plain.SHA256
	st.EncryptedSize, st.EncryptedSHA256 = stored.CipherSize, "sha256:"+stored.CipherSHA256
	st.PageSize = int32(stored.Plain.PageSize)
	st.DatabaseTxID = int64(min(stored.Plain.TxID, uint64(1<<63-1)))
	set(osv1alpha1.ConditionComplete, metav1.ConditionTrue, osv1alpha1.ReasonStored,
		fmt.Sprintf("%d bytes from %s, encrypted to %d recipient(s), stored at %s",
			st.Size, src.node, len(st.Recipients), st.Location))

	// Verified: what came off etcd is an intact snapshot, and what the
	// sink holds is exactly what was sent.
	if err := stored.Plain.Valid(); err != nil {
		set(osv1alpha1.ConditionVerified, metav1.ConditionFalse, osv1alpha1.ReasonDamaged, err.Error())
		return ctrl.Result{}, nil
	}
	size, sum, err := etcdbackup.ReadBack(ctx, sink, key)
	switch {
	case err != nil:
		set(osv1alpha1.ConditionVerified, metav1.ConditionFalse, osv1alpha1.ReasonReadBackFailed, err.Error())
	case size != stored.CipherSize || sum != stored.CipherSHA256:
		set(osv1alpha1.ConditionVerified, metav1.ConditionFalse, osv1alpha1.ReasonReadBackFailed, fmt.Sprintf(
			"the sink holds %d bytes with sha256 %s, not the %d bytes with sha256 %s that were sent",
			size, sum, stored.CipherSize, stored.CipherSHA256))
	default:
		set(osv1alpha1.ConditionVerified, metav1.ConditionTrue, osv1alpha1.ReasonIntact, fmt.Sprintf(
			"etcd's integrity hash matches, a bbolt database of %d pages of %d bytes (transaction %d), "+
				"and the sink holds the ciphertext sent", stored.Plain.PageCount, stored.Plain.PageSize, stored.Plain.TxID))
	}
	log.Info("etcd snapshot stored", "snapshot", s.Name, "from", src.node, "location", st.Location, "bytes", st.Size)
	return ctrl.Result{}, nil
}

func (r *EtcdSnapshotReconciler) policy(ctx context.Context, s *osv1alpha1.EtcdSnapshot) (*osv1alpha1.EtcdBackupPolicy, error) {
	p := &osv1alpha1.EtcdBackupPolicy{}
	return p, r.Get(ctx, types.NamespacedName{Name: s.Spec.PolicyRef.Name}, p)
}

type memberSource struct {
	node, endpoint string
	status         talos.EtcdStatus
}

// pickMember asks every control plane for its etcd status and picks the
// member to snapshot: a healthy voting member that follows a leader, a
// follower before the leader (the snapshot reads the whole database, which
// the leader should not spend its time on), by name among equals. why says
// what was wrong with each when none qualifies.
func (r *EtcdSnapshotReconciler) pickMember(ctx context.Context, talosconfig []byte,
	cps []osv1alpha1.TalosNode) (*memberSource, string) {
	var leaders, followers []memberSource
	var problems []string
	for _, n := range cps {
		endpoint := render.NodeAddress(&n)
		var es talos.EtcdStatus
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := talos.With(pctx, r.Dial, talos.Target{Endpoint: endpoint, Talosconfig: talosconfig}, func(c talos.Client) error {
			var err error
			es, err = c.EtcdStatus(pctx)
			return err
		})
		cancel()
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", n.Name, err))
		case len(es.Errors) > 0:
			problems = append(problems, fmt.Sprintf("%s: %s", n.Name, strings.Join(es.Errors, "; ")))
		case es.IsLearner:
			problems = append(problems, n.Name+": a learner, not yet in sync")
		case es.LeaderID == 0:
			problems = append(problems, n.Name+": knows no leader (quorum lost?)")
		case es.LeaderID == es.MemberID:
			leaders = append(leaders, memberSource{n.Name, endpoint, es})
		default:
			followers = append(followers, memberSource{n.Name, endpoint, es})
		}
	}
	if len(followers) > 0 {
		return &followers[0], ""
	}
	if len(leaders) > 0 {
		return &leaders[0], ""
	}
	if len(cps) == 0 {
		return nil, "the cluster has no control-plane TalosNode with an address"
	}
	return nil, strings.Join(problems, ", ")
}

// SetupWithManager wires both backup reconcilers: they are one lane.
func (b *EtcdBackups) SetupWithManager(mgr ctrl.Manager) error {
	if err := (&EtcdBackupPolicyReconciler{b}).SetupWithManager(mgr); err != nil {
		return err
	}
	return (&EtcdSnapshotReconciler{b}).SetupWithManager(mgr)
}

func (r *EtcdSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&osv1alpha1.EtcdSnapshot{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("etcdsnapshot").
		Complete(r)
}

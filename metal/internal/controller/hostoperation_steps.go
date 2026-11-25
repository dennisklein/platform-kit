package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/boot"
	"github.com/dennisklein/platform-kit/metal/internal/oci"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
)

// defaultCheckInTimeout mirrors the schema default for awaitCheckIn.
const defaultCheckInTimeout = 30 * time.Minute

// step converges action i to its goal, or reports what it waits for.
func (x *exec) step(ctx context.Context, i int) (stepState, error) {
	a := x.op.Spec.Actions[i]
	switch {
	case a.PowerCycle != nil:
		return x.powerCycle(ctx)
	case a.PowerOff != nil:
		return x.powerOff(ctx, a.PowerOff)
	case a.SetBootOverride != nil:
		return stepDone, x.setBootOverride(ctx, a.SetBootOverride)
	case a.InsertMedia != nil:
		err := x.insertMedia(ctx, i, a.InsertMedia)
		if pending := (*artifactPending)(nil); errors.As(err, &pending) {
			return waitingOn("%s", pending.msg), nil
		}
		return stepDone, err
	case a.EjectMedia != nil:
		cl, _, err := x.client(ctx)
		if err != nil {
			return stepState{}, err
		}
		return stepDone, boot.EjectAndVerify(ctx, cl, a.EjectMedia.MediaID, nil)
	case a.ApplyBios != nil:
		cl, sysID, err := x.client(ctx)
		if err != nil {
			return stepState{}, err
		}
		return stepDone, applyBios(ctx, cl, sysID, a.ApplyBios)
	case a.FlashFirmware != nil:
		cl, _, err := x.client(ctx)
		if err != nil {
			return stepState{}, err
		}
		return stepDone, x.flashFirmware(ctx, i, cl, a.FlashFirmware)
	case a.AwaitCheckIn != nil:
		return x.awaitCheckIn(ctx, a.AwaitCheckIn)
	case a.EtcdRecover != nil:
		return x.etcdRecover(ctx, a.EtcdRecover)
	case a.StageFirmware != nil:
		return stepDone, x.stageFirmware(ctx, i, a.StageFirmware)
	}
	if st, ok, err := x.talosStep(ctx, i); ok {
		return st, err
	}
	return stepState{}, fmt.Errorf("action carries no known key")
}

// waitStart is where a waiting step's budget counts from.
//
// A step that acts (a power transition) counts from the moment this process
// first looked at it, entered being when this pass entered it. Counting from
// the previous step's end would also count the time the operation sat in the
// queue, behind other passes, before anyone acted: a power-on that one settle
// window did not show would fail for good after a single look. A restart
// starts the count again, which can only make a step fail later.
//
// awaitCheckIn, waitNodeReady and waitEtcdHealthy act on nothing: the
// machine has been booting since the previous step ended, whether or not
// anyone looked, so their budget counts from there.
func (x *exec) waitStart(i int, entered time.Time) time.Time {
	if a := x.op.Spec.Actions[i]; a.AwaitCheckIn != nil || a.WaitNodeReady != nil || a.WaitEtcdHealthy != nil || a.EtcdRecover != nil {
		return x.stepStart(i)
	}
	key := types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name}
	return x.r.clock.since(key, x.op.UID, i, entered)
}

// stepStart is when step i began: the previous step's end, else the baseline.
func (x *exec) stepStart(i int) time.Time {
	if i > 0 && i <= len(x.op.Status.Steps) {
		if t := x.op.Status.Steps[i-1].FinishedAt; t != nil {
			return t.Time
		}
	}
	if b := x.op.Status.Baseline; b != nil && b.CapturedAt != nil {
		return b.CapturedAt.Time
	}
	return time.Now()
}

// stepBudget is how long step i may keep waiting before it fails. A graceful
// powerOff gets its graceful window on top of the forced one's.
func (x *exec) stepBudget(i int) time.Duration {
	if a := x.op.Spec.Actions[i].AwaitCheckIn; a != nil {
		return checkInTimeout(a)
	}
	if a := x.op.Spec.Actions[i].EtcdRecover; a != nil {
		return etcdRecoverTimeout(a)
	}
	if a := x.op.Spec.Actions[i].PowerOff; a != nil {
		return gracefulWindow(a) + x.r.powerTimeout()
	}
	if a := x.op.Spec.Actions[i].InsertMedia; a != nil && a.BootAssetRef == nil {
		// The image is downloaded into the media store first.
		return fetchTimeout + x.r.powerTimeout()
	}
	if d, ok := talosStepBudget(x.op.Spec.Actions[i]); ok {
		return d
	}
	return x.r.powerTimeout()
}

func gracefulWindow(a *metalv1alpha1.PowerOffAction) time.Duration {
	return time.Duration(a.GracefulTimeoutSeconds) * time.Second
}

func checkInTimeout(a *metalv1alpha1.AwaitCheckInAction) time.Duration {
	if a.TimeoutSeconds > 0 {
		return time.Duration(a.TimeoutSeconds) * time.Second
	}
	return defaultCheckInTimeout
}

// power reads the system's reported power state.
func (x *exec) power(ctx context.Context) (bmc.PowerState, error) {
	cl, sysID, err := x.client(ctx)
	if err != nil {
		return "", err
	}
	return cl.Power(ctx, sysID)
}

// settle polls the reported power state until it reads want or the pass's
// settle window ends; it returns the last reading.
func (x *exec) settle(ctx context.Context, want bmc.PowerState) (bmc.PowerState, error) {
	deadline := time.Now().Add(x.r.powerSettle())
	var (
		cur bmc.PowerState
		err error
	)
	// The end of ctx is not an error here: the caller reads the last
	// reading, and its own ctx check reports the cancellation.
	_ = poll.Until(ctx, poll.Every(500*time.Millisecond), func(ctx context.Context) (bool, error) {
		cur, err = x.power(ctx)
		return err != nil || cur == want || time.Now().After(deadline), nil
	})
	return cur, err
}

// powerTo converges the reported power state on want: already there is
// success; otherwise the action is issued once per pass and the pass waits a
// settle window for the state to show.
func (x *exec) powerTo(ctx context.Context, want bmc.PowerState) (stepState, error) {
	cur, err := x.power(ctx)
	if err != nil {
		return stepState{}, err
	}
	if cur == want {
		return stepDone, nil
	}
	action := bmc.ActionOn
	if want == bmc.PowerOff {
		action = bmc.ActionForceOff
	}
	// A machine on its way to want (PoweringOn, PoweringOff) got the action
	// already, in this pass or an earlier one: a second one could be
	// refused, or restart the transition.
	if cur != towards(want) {
		cl, sysID, _ := x.client(ctx)
		if err := cl.SetPower(ctx, sysID, action); err != nil {
			return stepState{}, err
		}
	}
	if cur, err = x.settle(ctx, want); err != nil {
		return stepState{}, err
	}
	if cur == want {
		return stepDone, nil
	}
	return waitingOn("%s issued, machine still reports %s", action, cur), nil
}

// towards is the transitional state a system reports on its way to want.
func towards(want bmc.PowerState) bmc.PowerState {
	if want == bmc.PowerOn {
		return bmc.PowerPoweringOn
	}
	return bmc.PowerPoweringOff
}

// powerOff turns the machine off. With a graceful window it first asks the OS
// to shut down, once (recorded in status, like the off half of a power
// cycle), and forces the machine off only when it still reports on after the
// window. A BMC that refuses the graceful request is forced at once: the
// person who approved a power-off did not approve a machine left running.
func (x *exec) powerOff(ctx context.Context, a *metalv1alpha1.PowerOffAction) (stepState, error) {
	window := gracefulWindow(a)
	if window <= 0 {
		return x.powerTo(ctx, bmc.PowerOff)
	}
	cur, err := x.power(ctx)
	if err != nil {
		return stepState{}, err
	}
	if cur == bmc.PowerOff {
		return stepDone, nil
	}
	if x.op.Status.ShutdownRequested == nil {
		cl, sysID, err := x.client(ctx)
		if err != nil {
			return stepState{}, err
		}
		if err := cl.SetPower(ctx, sysID, bmc.ActionGracefulShutdown); err != nil {
			if transient(ctx, err) {
				return stepState{}, err
			}
			logf.FromContext(ctx).Info("BMC refused a graceful shutdown, forcing the machine off",
				"operation", x.op.Name, "error", err.Error())
			return x.powerTo(ctx, bmc.PowerOff)
		}
		x.op.Status.ShutdownRequested = &metav1.MicroTime{Time: time.Now()}
		// Persist before waiting: a resumed pass must not ask again, nor
		// start the window over.
		if err := x.flush(); err != nil {
			return stepState{}, &persistError{err}
		}
		if cur, err = x.settle(ctx, bmc.PowerOff); err != nil {
			return stepState{}, err
		}
		if cur == bmc.PowerOff {
			return stepDone, nil
		}
	}
	asked := x.op.Status.ShutdownRequested.Time
	if left := window - time.Since(asked); left > 0 {
		return waitingOn("asked the OS to shut down at %s; forcing the machine off in %s if it is still on",
			asked.UTC().Format(time.RFC3339), left.Round(time.Second)), nil
	}
	return x.powerTo(ctx, bmc.PowerOff)
}

// powerCycle restarts the machine as two observed halves: off (recorded in
// status the moment it is seen), then on. A machine that was off at the
// baseline only needs the second half.
//
// Recording the off observation is what makes the step idempotent on every
// vendor. Without it, a pass resumed after the machine came back on cannot
// tell "cycled already" from "never cycled" where no reset time is served
// (iBMC) — and would cycle the machine a second time, into a boot that
// already used up its Once override.
func (x *exec) powerCycle(ctx context.Context) (stepState, error) {
	// A machine on its way on (PoweringOn) is cycled like one that is on.
	if b := x.op.Status.Baseline; b != nil &&
		b.PowerState != string(bmc.PowerOn) && b.PowerState != string(bmc.PowerPoweringOn) {
		return x.powerTo(ctx, bmc.PowerOn)
	}
	if x.op.Status.OffObserved == nil {
		st, err := x.powerTo(ctx, bmc.PowerOff)
		if err != nil || !st.done {
			return st, err
		}
		x.op.Status.OffObserved = &metav1.MicroTime{Time: time.Now()}
		// Persist before turning it on: from here on, a resumed pass must
		// only ever finish this cycle.
		if err := x.flush(); err != nil {
			return stepState{}, &persistError{err}
		}
	}
	return x.powerTo(ctx, bmc.PowerOn)
}

// setBootOverride converges the override on the wanted triple: an override
// already set this way is success and is not written again.
func (x *exec) setBootOverride(ctx context.Context, a *metalv1alpha1.SetBootOverrideAction) error {
	cl, sysID, err := x.client(ctx)
	if err != nil {
		return err
	}
	want := bmc.BootOverride{Target: a.Target, Enabled: bmc.OverrideOnce, Mode: a.Mode}
	if a.Once != nil && !*a.Once {
		want.Enabled = bmc.OverrideContinuous
	}
	sys, err := systemOf(ctx, cl, sysID)
	if err != nil {
		return err
	}
	cur := sys.BootOverride
	if cur.Target == want.Target && cur.Enabled == want.Enabled && (want.Mode == "" || cur.Mode == want.Mode) {
		return nil // goal already met
	}
	if len(sys.BootTargets) > 0 && !slices.Contains(sys.BootTargets, want.Target) {
		return fmt.Errorf("boot target %q not in AllowableValues %v", want.Target, sys.BootTargets)
	}
	return cl.SetBootOverride(ctx, sysID, want)
}

// insertMedia converges a slot on the wanted image. A slot holding something
// else is emptied first (an occupied X12 slot refuses InsertMedia).
func (x *exec) insertMedia(ctx context.Context, i int, a *metalv1alpha1.InsertMediaAction) error {
	cl, _, err := x.client(ctx)
	if err != nil {
		return err
	}
	if a.BootAssetRef == nil {
		return x.insertURL(ctx, cl, i, a)
	}

	ba := &osv1alpha1.BootAsset{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: a.BootAssetRef.Name}, ba); err != nil {
		return fmt.Errorf("boot asset %s: %w", a.BootAssetRef.Name, err)
	}
	// A medium pinned to the digest the approver saw needs no Ready: the
	// digest check at the grant is the rail, and it passes exactly when the
	// artifact approved is still the one on record. While a newer build
	// runs, the status keeps naming that artifact with Ready=False, and a
	// change in the build key alone (a new extension image digest) must not
	// fail every approved install that has not mounted yet. Only an unpinned
	// medium takes whatever is built now, and for that it must be Ready.
	pinned := a.Digest
	if pinned == "" && !meta.IsStatusConditionTrue(ba.Status.Conditions, osv1alpha1.ConditionReady) {
		return fmt.Errorf("boot asset %s is not Ready", ba.Name)
	}
	if pinned != "" && !hasArtifact(ba) {
		return &artifactPending{msg: fmt.Sprintf("boot asset %s has no artifact yet; the approved one (%s) is mounted once it is there",
			ba.Name, a.Digest)}
	}
	// An unpinned medium is pinned here, to the artifact the status names:
	// what the grant serves is checked against a digest either way.
	pinned = cmp.Or(pinned, ba.Status.Digest)

	if x.r.Media == nil {
		return fmt.Errorf("boot asset %s is served by the media service, and this operator runs none (--media-url)", ba.Name)
	}
	issue := func(handle, filename string, ttl time.Duration, creds *asset.Credentials) (*asset.Grant, error) {
		return x.grant(ctx, ba, pinned, handle, filename, ttl, creds)
	}
	return x.mountGranted(ctx, cl, i, a.MediaID, ba.Name+".iso", secretBearing(ba), issue)
}

// insertURL mounts an image named by URL and digest. The operator
// downloads it into the media store first, over TLS it verifies, and keeps
// it only when it has the digest; the BMC then fetches it from the media
// service, under a grant, like any other medium. The download belongs to
// the operation and goes when the operation ends (forget).
func (x *exec) insertURL(ctx context.Context, cl bmc.Client, i int, a *metalv1alpha1.InsertMediaAction) error {
	if a.Digest == "" {
		// Admission refuses this; an object from before the rule is
		// refused here too.
		return fmt.Errorf("image %s has no digest: the operator mounts an image by URL only with the digest it checks", redact.String(a.ImageURL))
	}
	if x.r.Media == nil || x.r.Store == nil {
		return fmt.Errorf("image %s is downloaded into the media store and served by the media service, and this operator runs no media service (--media-url)",
			redact.String(a.ImageURL))
	}
	key := types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name}
	name, sum := urlStoreName(key), strings.TrimPrefix(a.Digest, "sha256:")
	if !x.r.Store.Has(name, sum) {
		st := x.r.fetcher().start(x.r.Store, x.r.imageClient(), a.ImageURL, name, sum, a.Digest)
		switch {
		case st.running:
			return &artifactPending{msg: fmt.Sprintf("the operator downloads %s into its media store", redact.String(a.ImageURL))}
		case st.err != nil:
			return fmt.Errorf("download %s: %w", redact.String(a.ImageURL), st.err)
		}
	}
	issue := func(handle, filename string, ttl time.Duration, _ *asset.Credentials) (*asset.Grant, error) {
		f, err := x.r.Store.Open(name, sum, a.Digest)
		if err != nil {
			return nil, fmt.Errorf("image %s in the media store: %w", redact.String(a.ImageURL), err)
		}
		return x.r.Media.Grants.IssueFile(handle, filename, f, ttl, nil)
	}
	return x.mountGranted(ctx, cl, i, a.MediaID, urlGrantFile(a.Digest), false, issue)
}

// urlStoreName is where the media store keeps the images an operation
// downloaded: one directory per operation, removed when it ends.
func urlStoreName(op types.NamespacedName) string {
	return urlStorePrefix + hash8(digest.FromString(op.String()).String())
}

// urlStorePrefix starts the store names of operation downloads; the store
// sweep keeps those of the operations that exist.
const urlStorePrefix = "op-"

// urlGrantFile is the grant filename of an image named by URL: its digest
// names it, so two images of one operation get two grants.
func urlGrantFile(digest string) string {
	return "image-" + strings.TrimPrefix(digest, "sha256:")[:16] + ".iso"
}

// mountGranted converges a slot on a medium the media service serves under
// a grant of the operation: a live grant whose URL the slot holds is the
// goal met; anything else is ejected and mounted under a fresh grant, which
// issue mints. withCreds gives the grant its own user and password, for a
// medium that carries a secret.
func (x *exec) mountGranted(ctx context.Context, cl bmc.Client, i int, slot, filename string, withCreds bool,
	issue func(handle, filename string, ttl time.Duration, creds *asset.Credentials) (*asset.Grant, error)) error {
	media := x.r.Media
	handle := grantHandle(types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name})
	if g, ok := media.Grants.Live(handle, filename); ok {
		if mounted(ctx, cl, slot) == media.URL(g) {
			return nil // mounted under a grant that still answers
		}
		media.Grants.Revoke(g.Token)
	}
	// Either never mounted, or mounted under a grant a restart forgot: the
	// grant table fails closed, so the mount is re-minted, never resurrected.
	if err := boot.EjectAndVerify(ctx, cl, slot, nil); err != nil {
		return err
	}
	// A medium that carries a config gets its own user and password as
	// well. One without a secret (an installer) needs none: the token, the
	// source binding and the digest pin are its rails, and on the vmm
	// dialect no credentials sit in the URL the BMC keeps.
	var creds *asset.Credentials
	if withCreds {
		var err error
		if creds, err = asset.MintCredentials(); err != nil {
			return err
		}
	}
	g, err := issue(handle, filename, x.grantTTL(i), creds)
	if err != nil {
		return err
	}
	logf.FromContext(ctx).Info("mounting granted medium", "operation", x.op.Name, "medium", filename,
		"slot", slot, "expires", g.ExpiresAt)
	var bmcCreds *bmc.MediaCredentials
	if creds != nil {
		bmcCreds = &bmc.MediaCredentials{Username: creds.Username, Password: creds.Password}
	}
	if err := cl.InsertMedia(ctx, slot, media.URL(g), bmcCreds); err != nil {
		media.Grants.Revoke(g.Token)
		return err
	}
	return nil
}

// remount mounts again every medium a recorded step mounted and the slot no
// longer holds as mounted, before the machine boots from it. The step that
// mounted it is never re-entered, and two things take a mount away behind
// its back:
//
//   - Grants live in memory only: after a restart or a leader change the slot
//     still holds a URL that the media service refuses. The medium is
//     mounted again under a fresh grant.
//   - A manager restart (a firmware update, a reset by a person) empties the
//     slots on most vendors, while the grant is still live here.
//
// So the slot is read, and the medium re-converged when the slot does not
// hold the URL it should, whichever the cause. Once the power cycle is
// recorded the boot has happened, and the media are left alone.
func (x *exec) remount(ctx context.Context, recorded int) error {
	actions := x.op.Spec.Actions[:recorded]
	for _, a := range actions {
		if a.PowerCycle != nil {
			return nil
		}
	}
	for j, a := range actions {
		in := a.InsertMedia
		if in == nil || slotReusedLater(actions[j+1:], in.MediaID) {
			continue
		}
		if ok, err := x.stillMounted(ctx, in); err != nil || ok {
			return err
		}
		logf.FromContext(ctx).Info("re-mounting a medium the slot no longer holds",
			"operation", x.op.Name, "slot", in.MediaID)
		if err := x.insertMedia(ctx, j, in); err != nil {
			return fmt.Errorf("re-mount on %s: %w", in.MediaID, err)
		}
	}
	return nil
}

// stillMounted: the slot holds what a recorded insertMedia mounted, under a
// grant this process still serves. A medium is compared with its live
// grant's URL, so one mounted under a grant a restart forgot reads as not
// mounted.
func (x *exec) stillMounted(ctx context.Context, in *metalv1alpha1.InsertMediaAction) (bool, error) {
	cl, _, err := x.client(ctx)
	if err != nil {
		return false, err
	}
	cur, err := mountedURL(ctx, cl, in.MediaID)
	if err != nil {
		return false, err
	}
	filename := urlGrantFile(in.Digest)
	if in.BootAssetRef != nil {
		filename = in.BootAssetRef.Name + ".iso"
	} else if in.Digest == "" {
		return false, nil // insertMedia refuses it, saying why
	}
	media := x.r.Media
	if media == nil {
		// Nothing here could have mounted it; insertMedia says so.
		return false, nil
	}
	handle := grantHandle(types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name})
	g, ok := media.Grants.Live(handle, filename)
	return ok && cur == media.URL(g), nil
}

// slotReusedLater: one of the actions empties the slot or mounts something
// else in it. Either way the earlier medium is no longer what the slot is
// meant to hold, and mounting it again would put back what a recorded step
// took out: the machine would boot from it.
func slotReusedLater(actions []metalv1alpha1.HostOperationAction, slot string) bool {
	return slices.ContainsFunc(actions, func(a metalv1alpha1.HostOperationAction) bool {
		return (a.EjectMedia != nil && a.EjectMedia.MediaID == slot) ||
			(a.InsertMedia != nil && a.InsertMedia.MediaID == slot)
	})
}

// artifactPending: the asset names no built artifact yet. The step waits for
// one instead of failing: whether it is the approved one is checked at the
// grant, by digest.
type artifactPending struct{ msg string }

func (e *artifactPending) Error() string { return e.msg }

// hasArtifact: the status names a built artifact, for the asset's strategy.
func hasArtifact(ba *osv1alpha1.BootAsset) bool {
	if storeBacked(ba) {
		return ba.Status.BuildKey != ""
	}
	return ba.Status.ArtifactSecretRef != nil
}

// storeBacked: the asset's artifact rests in the media store, under its
// build key: an embedded installer the imager built, or a factory installer
// the operator downloaded.
func storeBacked(ba *osv1alpha1.BootAsset) bool {
	return ba.Spec.Strategy == osv1alpha1.StrategyEmbeddedInstaller || ba.Spec.Strategy == osv1alpha1.StrategyFactory
}

// secretBearing: the asset carries a node's config and is only ever served
// under a grant.
func secretBearing(ba *osv1alpha1.BootAsset) bool {
	return ba.Spec.Strategy == osv1alpha1.StrategyConfigISO || ba.Spec.Strategy == osv1alpha1.StrategyEmbeddedInstaller
}

// grant mints the grant for an asset's artifact, checked against the
// digest the operation was approved with: a config medium from its Secret,
// an installer from the media store, served from the file it opened and
// checked.
func (x *exec) grant(ctx context.Context, ba *osv1alpha1.BootAsset, pinned, handle, filename string,
	ttl time.Duration, creds *asset.Credentials) (*asset.Grant, error) {
	media := x.r.Media
	if storeBacked(ba) {
		if x.r.Store == nil {
			return nil, fmt.Errorf("boot asset %s rests in the media store and this operator runs none (--media-store)", ba.Name)
		}
		if ba.Status.BuildKey == "" {
			return nil, fmt.Errorf("boot asset %s has no artifact yet", ba.Name)
		}
		f, err := x.r.Store.Open(ba.Name, ba.Status.BuildKey, pinned)
		if err != nil {
			return nil, fmt.Errorf("boot asset %s was rebuilt since approval or is unreadable: %w", ba.Name, err)
		}
		return media.Grants.IssueFile(handle, filename, f, ttl, creds)
	}
	iso, err := x.artifact(ctx, ba, pinned)
	if err != nil {
		return nil, err
	}
	return media.Grants.Issue(handle, filename, iso, ttl, creds)
}

// artifact loads a configISO BootAsset's built bytes and checks them against
// the pinned digest — the approver saw that digest, so anything else is
// content nobody approved.
func (x *exec) artifact(ctx context.Context, ba *osv1alpha1.BootAsset, pinned string) ([]byte, error) {
	ref := ba.Status.ArtifactSecretRef
	if ref == nil {
		return nil, fmt.Errorf("boot asset %s has no artifact yet", ba.Name)
	}
	sec := &corev1.Secret{}
	if err := x.r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("artifact secret %s/%s not found", ref.Namespace, ref.Name)
		}
		return nil, err
	}
	iso := sec.Data[artifactKey]
	if len(iso) == 0 {
		return nil, fmt.Errorf("artifact secret %s/%s holds no %s", ref.Namespace, ref.Name, artifactKey)
	}
	got := digest.FromBytes(iso).String()
	if pinned != "" && got != pinned {
		return nil, fmt.Errorf("boot asset %s was rebuilt since approval (artifact %s, approved %s)", ba.Name, got, pinned)
	}
	return iso, nil
}

// grantTTL bounds a grant minted at step i by the waits still ahead of it.
func (x *exec) grantTTL(i int) time.Duration {
	ttl := grantMargin + x.r.powerTimeout()
	for _, a := range x.op.Spec.Actions[i:] {
		if a.AwaitCheckIn != nil {
			ttl += checkInTimeout(a.AwaitCheckIn)
		}
	}
	return ttl
}

// mounted is the image URL a slot holds, userinfo stripped (the vmm dialect
// stores it credential-bearing); empty when empty or unreadable.
func mounted(ctx context.Context, cl bmc.Client, slot string) string {
	u, _ := mountedURL(ctx, cl, slot)
	return u
}

// mountedURL is mounted with the read error kept: a caller that would mount
// again on an empty answer must not take an unreadable slot for an empty one.
func mountedURL(ctx context.Context, cl bmc.Client, slot string) (string, error) {
	media, err := cl.Media(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range media {
		if m.ID == slot && m.Inserted {
			u, err := url.Parse(m.ImageURL)
			if err != nil {
				return m.ImageURL, nil
			}
			u.User = nil
			return u.String(), nil
		}
	}
	return "", nil
}

// awaitCheckIn waits for the node to run its config: the authenticated
// machine API answering, or the node reporting Ready. The check-in ends the
// operation's media grants.
func (x *exec) awaitCheckIn(ctx context.Context, a *metalv1alpha1.AwaitCheckInAction) (stepState, error) {
	node := &osv1alpha1.TalosNode{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: a.NodeRef.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return waitingOn("TalosNode %s not found", a.NodeRef.Name), nil
		}
		return stepState{}, err
	}
	if nodeCheckedIn(node) {
		if stale, err := x.staleCheckIn(ctx, node); err != nil || stale {
			return waitingOn("waiting for TalosNode %s to check in after its reinstall", node.Name), err
		}
		// The machine runs its config: the media that carried the config
		// and its secrets have done their job. Their grants end now, not
		// when the operation ends, which can be much later.
		x.r.revokeGrants(types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name})
		return stepDone, nil
	}
	seen := "no answer from the machine yet"
	if node.Status.Stage != "" {
		seen = "last stage seen: " + node.Status.Stage
	}
	st := waitingOn("waiting for TalosNode %s to check in (%s)", node.Name, seen)
	st.hint = checkInHint(x.host, node)
	return st, nil
}

// checkInHint says where to look when a machine never checks in: what a
// kickstart admin opens first, the console, then what the machine got from
// the network if it sits in Talos' maintenance mode.
func checkInHint(host *metalv1alpha1.Host, node *osv1alpha1.TalosNode) string {
	machine := "<machine>"
	if a := host.Labels[labelAsset]; a != "" {
		machine = a
	}
	hint := fmt.Sprintf(". Look at the machine's console: the BMC's serial-over-LAN or remote console "+
		"(in the virtual lab: .lab/state/%s/console.log)", machine)
	addr := node.Status.Address
	if addr == "" {
		addr = node.Spec.Address
	}
	if addr != "" {
		hint += fmt.Sprintf(". If it waits in maintenance mode, see which address it got: talosctl -n %s --insecure get addresses", addr)
	}
	return hint
}

// nodeCheckedIn: the machine runs the node's config — the authenticated API
// answers (our PKI), or the node is Ready.
func nodeCheckedIn(node *osv1alpha1.TalosNode) bool {
	if c := meta.FindStatusCondition(node.Status.Conditions, osv1alpha1.ConditionMachineReachable); c != nil &&
		c.Status == metav1.ConditionTrue && c.Reason == osv1alpha1.ReasonMachineAPI {
		return true
	}
	return meta.IsStatusConditionTrue(node.Status.Conditions, osv1alpha1.ConditionReady)
}

// applyBios stages the pinned attributes that are not current yet. The
// attributes come from the operation itself, never from the live
// BiosSettings: what runs is what was approved. An attribute already current
// and with nothing contradictory pending is done.
//
// An attribute already staged with the desired value is sent again with the
// rest. Staging is not only the value: on iDRAC a value staged without an
// apply time gets no configuration job, and the reset that follows applies
// nothing. The client asks for OnReset where the service lists it, so
// sending the value again is what creates the job for a value an earlier
// client staged without one. Elsewhere the same value staged twice is the
// same staging.
//
// Sending it again can be refused where a job already holds it (a pass
// resumed after its own PATCH landed). When every value sent was already
// staged right, that refusal is not a failure: the goal was met before the
// call, as far as anything can read it.
func applyBios(ctx context.Context, cl bmc.Client, systemID string, a *metalv1alpha1.ApplyBiosAction) error {
	if len(a.Attributes) == 0 {
		return fmt.Errorf("applyBios carries no attributes")
	}
	state, err := cl.Bios(ctx, systemID)
	if err != nil {
		return err
	}
	stage := map[string]any{}
	restaged := true // every value in stage is already staged right
	for k, want := range decodeAttrs(a.Attributes) {
		staged, stagedOK := state.Pending[k]
		switch {
		case stagedOK && jsonEqual(staged, want):
			stage[k] = want // staged right: sent again, see above
		case stagedOK:
			// A contradictory value sits pending (a stale earlier staging) —
			// overwrite it, or the paired reboot applies the wrong one.
			stage[k], restaged = want, false
		case jsonEqual(state.Attributes[k], want):
			continue // already current, nothing pending
		default:
			stage[k], restaged = want, false
		}
	}
	if len(stage) == 0 {
		return nil // goal already met
	}
	err = cl.SetBiosAttributes(ctx, systemID, stage)
	if err != nil && restaged && !transient(ctx, err) {
		logf.FromContext(ctx).Info("BMC refused to stage again values already staged; taking them as staged",
			"error", err.Error())
		return nil
	}
	return err
}

// flashFirmware pushes the image and waits for the observable proof — the
// version reading back after the manager's own reboot. Acceptance is NOT
// completion (this firmware answers success to actions it silently declines),
// so the upload's return value proves nothing; the goal check does.
//
// The upload is recorded before it is sent. A resumed pass that finds the
// record only waits for the version: a second upload to a manager still
// busy with the first can wedge it (the X12's "already in update mode").
func (x *exec) flashFirmware(ctx context.Context, i int, cl bmc.Client, a *metalv1alpha1.FlashFirmwareAction) error {
	before, err := cl.Manager(ctx)
	if err != nil {
		return err
	}
	if before.FirmwareVersion == a.Version {
		return nil // goal already met
	}
	sent := x.upload(i)
	if sent == nil {
		image, err := oci.Pull(ctx, x.r.RegistryClient, a.Image)
		if err != nil {
			return err
		}
		sent = &metalv1alpha1.FirmwareUploadProgress{Step: int32(i), SentAt: metav1.MicroTime{Time: time.Now()}}
		if !before.LastResetTime.IsZero() {
			sent.ManagerResetTime = &metav1.MicroTime{Time: before.LastResetTime}
		}
		if err := x.recordUpload(sent); err != nil {
			return err
		}
		if err := cl.UpdateFirmware(ctx, image); err != nil {
			return err
		}
	}
	if err := poll.Until(ctx, poll.Every(2*time.Second), func(ctx context.Context) (bool, error) {
		// Mid-flash reads fail while the BMC reboots — expected, keep polling.
		m, err := cl.Manager(ctx)
		return err == nil && m.FirmwareVersion == a.Version &&
			(sent.ManagerResetTime == nil || m.LastResetTime.After(sent.ManagerResetTime.Time)), nil
	}); err != nil {
		return fmt.Errorf("upload sent at %s but version %s never read back: %w",
			sent.SentAt.UTC().Format(time.RFC3339), a.Version, err)
	}
	return nil
}

// upload is the recorded upload of step i, nil when step i sent none.
func (x *exec) upload(i int) *metalv1alpha1.FirmwareUploadProgress {
	if p := x.op.Status.UploadAccepted; p != nil && int(p.Step) == i {
		return p
	}
	return nil
}

// recordUpload persists the upload record before the upload is sent.
func (x *exec) recordUpload(p *metalv1alpha1.FirmwareUploadProgress) error {
	x.op.Status.UploadAccepted = p
	if x.flush == nil {
		return nil
	}
	if err := x.flush(); err != nil {
		return &persistError{err}
	}
	return nil
}

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"slices"
	"time"

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
		return x.powerTo(ctx, bmc.PowerOff)
	case a.SetBootOverride != nil:
		return stepDone, x.setBootOverride(ctx, a.SetBootOverride)
	case a.InsertMedia != nil:
		return stepDone, x.insertMedia(ctx, i, a.InsertMedia)
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
		return stepDone, x.r.flashFirmware(ctx, cl, a.FlashFirmware)
	case a.AwaitCheckIn != nil:
		return x.awaitCheckIn(ctx, a.AwaitCheckIn)
	}
	return stepState{}, fmt.Errorf("action carries no known key")
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

// stepBudget is how long step i may keep waiting before it fails.
func (x *exec) stepBudget(i int) time.Duration {
	if a := x.op.Spec.Actions[i].AwaitCheckIn; a != nil {
		return checkInTimeout(a)
	}
	return x.r.powerTimeout()
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
	for {
		cur, err := x.power(ctx)
		if err != nil || cur == want || time.Now().After(deadline) {
			return cur, err
		}
		select {
		case <-ctx.Done():
			return cur, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
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
	cl, sysID, _ := x.client(ctx)
	if err := cl.SetPower(ctx, sysID, action); err != nil {
		return stepState{}, err
	}
	if cur, err = x.settle(ctx, want); err != nil {
		return stepState{}, err
	}
	if cur == want {
		return stepDone, nil
	}
	return waitingOn("%s issued, machine still reports %s", action, cur), nil
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
	if b := x.op.Status.Baseline; b != nil && b.PowerState != string(bmc.PowerOn) {
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
			return stepState{}, err
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
		if mounted(ctx, cl, a.MediaID) == a.ImageURL {
			return nil // goal already met
		}
		if err := boot.EjectAndVerify(ctx, cl, a.MediaID, nil); err != nil {
			return err
		}
		return cl.InsertMedia(ctx, a.MediaID, a.ImageURL, nil)
	}

	ba := &osv1alpha1.BootAsset{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: a.BootAssetRef.Name}, ba); err != nil {
		return fmt.Errorf("boot asset %s: %w", a.BootAssetRef.Name, err)
	}
	if !meta.IsStatusConditionTrue(ba.Status.Conditions, osv1alpha1.ConditionReady) {
		return fmt.Errorf("boot asset %s is not Ready", ba.Name)
	}
	if ba.Spec.Strategy != osv1alpha1.StrategyConfigISO {
		// Secret-free: the stable URL, no grant, no credentials.
		if ba.Status.URL == "" {
			return fmt.Errorf("boot asset %s carries no url", ba.Name)
		}
		if mounted(ctx, cl, a.MediaID) == ba.Status.URL {
			return nil
		}
		if err := boot.EjectAndVerify(ctx, cl, a.MediaID, nil); err != nil {
			return err
		}
		return cl.InsertMedia(ctx, a.MediaID, ba.Status.URL, nil)
	}

	media := x.r.Media
	if media == nil {
		return fmt.Errorf("boot asset %s is secret-bearing and this operator runs no media service (--media-url)", ba.Name)
	}
	iso, err := x.artifact(ctx, ba, a.Digest)
	if err != nil {
		return err
	}
	handle := grantHandle(types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name})
	filename := ba.Name + ".iso"
	if g, ok := media.Grants.Live(handle, filename); ok {
		if mounted(ctx, cl, a.MediaID) == media.URL(g) {
			return nil // mounted under a grant that still answers
		}
		media.Grants.Revoke(g.Token)
	}
	// Either never mounted, or mounted under a grant a restart forgot: the
	// grant table fails closed, so the mount is re-minted, never resurrected.
	if err := boot.EjectAndVerify(ctx, cl, a.MediaID, nil); err != nil {
		return err
	}
	creds, err := asset.MintCredentials()
	if err != nil {
		return err
	}
	g, err := media.Grants.Issue(handle, filename, iso, x.grantTTL(i), creds)
	if err != nil {
		return err
	}
	logf.FromContext(ctx).Info("mounting granted medium", "operation", x.op.Name, "bootAsset", ba.Name,
		"slot", a.MediaID, "expires", g.ExpiresAt)
	if err := cl.InsertMedia(ctx, a.MediaID, media.URL(g),
		&bmc.MediaCredentials{Username: creds.Username, Password: creds.Password}); err != nil {
		media.Grants.Revoke(g.Token)
		return err
	}
	return nil
}

// remountGrants mounts again, under fresh grants, every granted medium a
// recorded step mounted and this process no longer serves. Grants live in
// memory only: after a restart or a leader change the slot still holds a URL
// that answers 403, and the step that mounted it is never re-entered. Until
// the machine is booted that dead URL is what it would boot from; once the
// power cycle is recorded the boot has happened and the medium is left alone.
func (x *exec) remountGrants(ctx context.Context, recorded int) error {
	media := x.r.Media
	if media == nil {
		return nil
	}
	actions := x.op.Spec.Actions[:recorded]
	for _, a := range actions {
		if a.PowerCycle != nil {
			return nil
		}
	}
	handle := grantHandle(types.NamespacedName{Namespace: x.op.Namespace, Name: x.op.Name})
	for j, a := range actions {
		in := a.InsertMedia
		if in == nil || in.BootAssetRef == nil || ejectedLater(actions[j+1:], in.MediaID) {
			continue
		}
		if _, ok := media.Grants.Live(handle, in.BootAssetRef.Name+".iso"); ok {
			continue
		}
		ba := &osv1alpha1.BootAsset{}
		if err := x.r.Get(ctx, client.ObjectKey{Name: in.BootAssetRef.Name}, ba); err != nil {
			return fmt.Errorf("boot asset %s: %w", in.BootAssetRef.Name, err)
		}
		if ba.Spec.Strategy != osv1alpha1.StrategyConfigISO {
			continue // a stable URL, nothing granted
		}
		logf.FromContext(ctx).Info("re-mounting a medium whose grant this process no longer holds",
			"operation", x.op.Name, "slot", in.MediaID)
		if err := x.insertMedia(ctx, j, in); err != nil {
			return fmt.Errorf("re-mount %s on %s: %w", ba.Name, in.MediaID, err)
		}
	}
	return nil
}

// ejectedLater: one of the actions empties the slot.
func ejectedLater(actions []metalv1alpha1.HostOperationAction, slot string) bool {
	return slices.ContainsFunc(actions, func(a metalv1alpha1.HostOperationAction) bool {
		return a.EjectMedia != nil && a.EjectMedia.MediaID == slot
	})
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
	got := fmt.Sprintf("sha256:%x", sha256.Sum256(iso))
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
	media, err := cl.Media(ctx)
	if err != nil {
		return ""
	}
	for _, m := range media {
		if m.ID == slot && m.Inserted {
			u, err := url.Parse(m.ImageURL)
			if err != nil {
				return m.ImageURL
			}
			u.User = nil
			return u.String()
		}
	}
	return ""
}

// awaitCheckIn waits for the node to run its config: the authenticated
// machine API answering, or the node reporting Ready.
func (x *exec) awaitCheckIn(ctx context.Context, a *metalv1alpha1.AwaitCheckInAction) (stepState, error) {
	node := &osv1alpha1.TalosNode{}
	if err := x.r.Get(ctx, client.ObjectKey{Name: a.NodeRef.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return waitingOn("TalosNode %s not found", a.NodeRef.Name), nil
		}
		return stepState{}, err
	}
	if nodeCheckedIn(node) {
		return stepDone, nil
	}
	return waitingOn("waiting for TalosNode %s to check in", node.Name), nil
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

// applyBios stages the pinned attributes' drifted subset into the pending
// object. Converge-to-goal at key granularity: an attribute already current or
// already staged with the desired value is done, not a conflict — a resumed
// operation PATCHes only what is left, and a fully-staged machine PATCHes
// nothing (the paired powerCycle then applies it). The attributes come from
// the operation itself, never from the live BiosSettings: what runs is what
// was approved.
func applyBios(ctx context.Context, cl bmc.Client, systemID string, a *metalv1alpha1.ApplyBiosAction) error {
	if len(a.Attributes) == 0 {
		return fmt.Errorf("applyBios carries no attributes")
	}
	state, err := cl.Bios(ctx, systemID)
	if err != nil {
		return err
	}
	stage := map[string]any{}
	for k, want := range decodeAttrs(a.Attributes) {
		staged, stagedOK := state.Pending[k]
		switch {
		case stagedOK && jsonEqual(staged, want):
			continue // already staged right
		case stagedOK:
			// A contradictory value sits pending (a stale earlier staging) —
			// overwrite it, or the paired reboot applies the wrong one.
			stage[k] = want
		case jsonEqual(state.Attributes[k], want):
			continue // already current, nothing pending
		default:
			stage[k] = want
		}
	}
	if len(stage) == 0 {
		return nil // goal already met (or fully staged)
	}
	return cl.SetBiosAttributes(ctx, systemID, stage)
}

// flashFirmware pushes the image and waits for the observable proof — the
// version reading back after the manager's own reboot. Acceptance is NOT
// completion (this firmware answers success to actions it silently declines),
// so the upload's return value proves nothing; the goal check does.
func (r *HostOperationReconciler) flashFirmware(ctx context.Context, cl bmc.Client, a *metalv1alpha1.FlashFirmwareAction) error {
	before, err := cl.Manager(ctx)
	if err != nil {
		return err
	}
	if before.FirmwareVersion == a.Version {
		return nil // goal already met
	}
	image, err := oci.Pull(ctx, r.RegistryClient, a.Image)
	if err != nil {
		return err
	}
	if err := cl.UpdateFirmware(ctx, image); err != nil {
		return err
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		// Mid-flash reads fail while the BMC reboots — expected, keep polling.
		m, err := cl.Manager(ctx)
		if err == nil && m.FirmwareVersion == a.Version &&
			(before.LastResetTime.IsZero() || m.LastResetTime.After(before.LastResetTime)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("upload accepted but version %s never read back: %w", a.Version, ctx.Err())
		case <-tick.C:
		}
	}
}

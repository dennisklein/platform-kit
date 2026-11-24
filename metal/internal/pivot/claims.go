package pivot

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClaimsMode says what the move does with the PersistentVolumeClaims the
// manager mounts (the media store).
type ClaimsMode string

const (
	// ClaimsCopy, the default, makes each claim on the target from the
	// source claim's spec, when the target has the claim already or a
	// StorageClass to provision it; otherwise the move refuses.
	ClaimsCopy ClaimsMode = "copy"
	// ClaimsEphemeral mounts an emptyDir on the target in place of each
	// claim. What the volume held does not move and is lost whenever the
	// manager pod restarts.
	ClaimsEphemeral ClaimsMode = "ephemeral"
)

// storeClaimFlag is the manager flag that names the media store's claim to
// the imager Jobs, which mount the same claim (cmd/manager).
const storeClaimFlag = "--media-store-claim"

// The annotations that mark a cluster's default StorageClass; the beta one is
// still honoured by the admission plugin.
var defaultClassAnnotations = []string{
	"storageclass.kubernetes.io/is-default-class",
	"storageclass.beta.kubernetes.io/is-default-class",
}

// ClaimAction is what the move does with one claim.
type ClaimAction int

const (
	// ClaimUse: the target has the claim already; it is used as it is.
	ClaimUse ClaimAction = iota
	// ClaimCreate: the move makes the claim on the target.
	ClaimCreate
	// ClaimEphemeral: the target manager mounts an emptyDir instead.
	ClaimEphemeral
)

// ClaimMove is one claim the manager's Deployment mounts, and how it gets to
// the target.
type ClaimMove struct {
	// Volume is the Deployment's volume that names the claim.
	Volume string
	// Name is the claim, in the operator namespace.
	Name   string
	Action ClaimAction
	// StorageClass, for ClaimCreate, is the class the target claim names:
	// the source's when the target has it, empty for the target's default.
	StorageClass string
	// DefaultClass is the target's default class, which provisions a
	// claim made without a class.
	DefaultClass string
	// source is the claim as the source has it.
	source *corev1.PersistentVolumeClaim
}

// String is the plan's line for the claim.
func (c ClaimMove) String() string {
	ref := "PersistentVolumeClaim " + c.Name
	switch c.Action {
	case ClaimUse:
		return ref + " is on the target already and is used as it is; what the source's volume holds does not move"
	case ClaimCreate:
		class := "StorageClass " + c.StorageClass
		if c.StorageClass == "" {
			class = "the default StorageClass " + c.DefaultClass
		}
		return fmt.Sprintf("%s (%s) is made on the target from %s, empty: what the source's volume holds does not move", ref, c.size(), class)
	default:
		return fmt.Sprintf("%s is replaced by an emptyDir on the target (--media-store=ephemeral): it starts empty and is emptied "+
			"whenever the manager pod restarts, and %s is taken off the manager, so nothing is built into it", ref, storeClaimFlag)
	}
}

func (c ClaimMove) size() string {
	if c.source != nil {
		if q, ok := c.source.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			return q.String()
		}
	}
	return "no size requested"
}

// claimMoves finds the claims the source manager mounts and decides, for
// each, how it gets to the target. It refuses when the target can neither
// provide a claim nor was told to do without.
func (m *Mover) claimMoves(ctx context.Context) ([]ClaimMove, error) {
	o := m.Options
	src, dst := m.Source.Client, m.Target.Client
	d := &appsv1.Deployment{}
	if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Deployment}, d); err != nil {
		return nil, err
	}
	var classes *storagev1.StorageClassList
	var out []ClaimMove
	var problems []string
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		c := ClaimMove{Volume: v.Name, Name: v.PersistentVolumeClaim.ClaimName, source: &corev1.PersistentVolumeClaim{}}
		if err := src.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: c.Name}, c.source); err != nil {
			return nil, fmt.Errorf("deployment %s/%s mounts PersistentVolumeClaim %s (volume %s), which %s does not have: %w",
				o.Namespace, o.Deployment, c.Name, c.Volume, m.Source.Name, err)
		}
		if o.Claims == ClaimsEphemeral {
			c.Action = ClaimEphemeral
			out = append(out, c)
			continue
		}
		if classes == nil {
			classes = &storagev1.StorageClassList{}
			if err := dst.List(ctx, classes); err != nil {
				return nil, fmt.Errorf("list the StorageClasses of %s: %w", m.Target.Name, err)
			}
		}
		c.DefaultClass = defaultClass(classes.Items)
		has := func(name string) bool {
			return slices.ContainsFunc(classes.Items, func(sc storagev1.StorageClass) bool { return sc.Name == name })
		}

		there := &corev1.PersistentVolumeClaim{}
		err := dst.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: c.Name}, there)
		switch {
		case err == nil:
			// A claim of a WaitForFirstConsumer class stays Pending until
			// the manager pod is scheduled; one that names a class the
			// target has will bind then.
			if there.Status.Phase == corev1.ClaimBound || (there.Spec.StorageClassName != nil && has(*there.Spec.StorageClassName)) {
				c.Action = ClaimUse
				out = append(out, c)
				continue
			}
			problems = append(problems, fmt.Sprintf("PersistentVolumeClaim %s/%s on %s is %s and names no StorageClass %s has: "+
				"bind it, or delete it so that the move makes it", o.Namespace, c.Name, m.Target.Name, there.Status.Phase, m.Target.Name))
			continue
		case !apierrors.IsNotFound(err):
			return nil, err
		}
		c.Action = ClaimCreate
		if sc := c.source.Spec.StorageClassName; sc != nil && *sc != "" && has(*sc) {
			c.StorageClass = *sc
		} else if c.DefaultClass == "" {
			problems = append(problems, fmt.Sprintf(
				"deployment %s/%s mounts PersistentVolumeClaim %s (volume %s), and %s has neither that claim nor a default "+
					"StorageClass to make it from. Give %s storage: mark a StorageClass as the default "+
					"(kubectl annotate storageclass <name> storageclass.kubernetes.io/is-default-class=true) or create the claim %s/%s there, "+
					"and run the move again. Or move with --media-store=ephemeral: the manager there mounts an emptyDir instead, "+
					"starts with an empty store, loses it on every restart, and builds no installer for a machine behind a one-slot BMC "+
					"until the claim exists (docs/metal/pivot.md)",
				o.Namespace, o.Deployment, c.Name, c.Volume, m.Target.Name, m.Target.Name, o.Namespace, c.Name))
			continue
		}
		out = append(out, c)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the operator cannot move now:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return out, nil
}

// defaultClass is the name of the default StorageClass, or "".
func defaultClass(classes []storagev1.StorageClass) string {
	for _, sc := range classes {
		for _, a := range defaultClassAnnotations {
			if sc.Annotations[a] == "true" {
				return sc.Name
			}
		}
	}
	return ""
}

// targetClaim is the claim the move makes on the target: the source's
// request only. The volume it bound to, its selector and its data source
// belong to the source cluster, and so does its class unless the target has
// one of that name.
func targetClaim(c ClaimMove) *corev1.PersistentVolumeClaim {
	s := c.source
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, Labels: s.Labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: s.Spec.AccessModes,
			Resources:   s.Spec.Resources,
			VolumeMode:  s.Spec.VolumeMode,
		},
	}
	if c.StorageClass != "" {
		pvc.Spec.StorageClassName = &c.StorageClass
	}
	return pvc
}

// ephemeralVolume is what replaces a claim on the target: an emptyDir no
// larger than the claim asked for, so the store cannot fill the node's disk.
func ephemeralVolume(c ClaimMove) corev1.VolumeSource {
	ed := &corev1.EmptyDirVolumeSource{}
	if q, ok := c.source.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		ed.SizeLimit = &q
	}
	return corev1.VolumeSource{EmptyDir: ed}
}

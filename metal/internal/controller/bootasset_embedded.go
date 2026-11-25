package controller

import (
	"context"
	"fmt"

	"github.com/opencontainers/go-digest"
	"github.com/siderolabs/talos/pkg/machinery/imager/quirks"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
)

// embed is the embeddedInstaller strategy: the node's rendered config and the
// install image's inputs in, an ISO in the media store out. The imager does
// the build in a Job; this reconciler starts it, waits for it, imports its
// output, and removes what newer builds superseded.
//
// The status describes the artifact in the store. While a newer build runs,
// it keeps naming the last one (Ready=False, Building): an operation approved
// against that artifact fails on the digest check if it is replaced, so
// nothing boots a medium nobody approved.
func (r *BootAssetReconciler) embed(ctx context.Context, ba *osv1alpha1.BootAsset) error {
	if r.Store == nil || r.Imager == nil {
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable,
			"this operator has no media store: start the manager with --media-store and --media-store-claim (the media-store kustomize component sets both)")
		return nil
	}
	if r.Imager.StoreClaim == "" {
		// An emptyDir store (pk-metal pivot --media-store=ephemeral): the
		// imager Jobs are other pods and cannot write into it.
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable, fmt.Sprintf(
			"this operator's media store (%s) is not on a PersistentVolumeClaim, so the imager Jobs cannot write into it and no installer is built: "+
				"give the cluster a StorageClass, create the claim, mount it at the store and pass --media-store-claim (https://dennisklein.github.io/platform-kit/reference/media-store/)",
			r.Store.Dir))
		return nil
	}
	if r.Imager.HelperImage == "" {
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable,
			"the manager does not know its own image, which the imager Jobs run: set --imager-helper-image")
		return nil
	}
	if !quirks.New(ba.Spec.TalosVersion).SupportsEmbeddedConfig() {
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable,
			fmt.Sprintf("Talos %s cannot read a config embedded in its image; that needs Talos 1.12 or later", ba.Spec.TalosVersion))
		return nil
	}
	node, machineConfig, err := r.nodeConfig(ctx, ba)
	if node == nil {
		return err
	}
	image, err := r.Imager.Image(ba.Spec.TalosVersion)
	if err != nil {
		r.hold(ba, osv1alpha1.ReasonBuilderUnavailable, err.Error())
		return nil
	}
	var names []string
	if ba.Spec.Schematic != nil {
		names = ba.Spec.Schematic.Customization.SystemExtensions.OfficialExtensions
	}
	extensions, err := r.Factory.ExtensionImages(ctx, ba.Spec.TalosVersion, names)
	if err != nil {
		r.hold(ba, osv1alpha1.ReasonSchematicUnavailable, err.Error())
		return nil
	}
	in := imagerInputs{
		ConfigDigest: digest.FromBytes(machineConfig).String(),
		TalosVersion: ba.Spec.TalosVersion,
		Image:        image,
		KernelArgs:   imagerKernelArgs(ba.Spec.Schematic),
		Extensions:   extensions,
	}
	key := in.key()
	jobName := imagerJobName(ba.Name, key)
	ba.Status.BuildJob = jobName

	if err := r.deleteStaleBuilds(ctx, ba, jobName); err != nil {
		return err
	}

	// The status may name an artifact the store does not hold: the store is
	// a new volume (the operator moved to another cluster, and the status
	// came along while the files did not) or an emptied one, or the file
	// there is not the one that was built. Either way it is built again,
	// and until then the status names no artifact, so that nothing plans
	// or grants a file that is not there.
	have := r.Store.Has(ba.Name, key)
	var sum string
	var size int64
	if have {
		if sum, size, err = r.Store.Digest(ba.Name, key); err != nil {
			return fmt.Errorf("read stored artifact of %s: %w", ba.Name, err)
		}
	}
	var gone string
	if built := ba.Status.BuildKey; built != "" && ba.Status.Digest != "" {
		switch {
		case built == key && have && sum != ba.Status.Digest:
			if err := r.Store.Remove(ba.Name, key); err != nil {
				return fmt.Errorf("remove the stored artifact of %s, which is not the one built: %w", ba.Name, err)
			}
			have = false
			gone = fmt.Sprintf("the stored installer was %s, not the %s that was built, and is removed", sum, ba.Status.Digest)
		case built == key && !have, built != key && !r.Store.Has(ba.Name, built):
			gone = fmt.Sprintf("the installer built as %s is not in the media store (a new or emptied volume, as after pk-metal pivot)", ba.Status.Digest)
		}
	}
	if gone != "" {
		logf.FromContext(ctx).Info("stored installer is gone", "bootAsset", ba.Name, "key", key, "why", gone)
		ba.Status.Digest, ba.Status.Size, ba.Status.BuildKey = "", 0, ""
		gone += "; building it again. "
	}

	if !have {
		job := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: jobName}, job)
		switch {
		case apierrors.IsNotFound(err):
			return r.startBuild(ctx, ba, node, jobName, in, gone)
		case err != nil:
			return err
		}
		done, ok, msg := jobFinished(job)
		switch {
		case !done:
			r.hold(ba, osv1alpha1.ReasonBuilding, fmt.Sprintf(
				"%simager Job %s/%s is building the installer for render %s. Its log: kubectl -n %s logs job/%s",
				gone, r.Namespace, jobName, short(in.ConfigDigest), r.Namespace, jobName))
			return nil
		case !ok:
			r.hold(ba, osv1alpha1.ReasonBuildFailed, fmt.Sprintf(
				"imager Job %s/%s failed: %s. Its log: kubectl -n %s logs job/%s. To build again: kubectl -n %s delete job %s",
				r.Namespace, jobName, msg, r.Namespace, jobName, r.Namespace, jobName))
			return nil
		}
		if sum, size, err = r.Store.Import(jobName, ba.Name, key); err != nil {
			r.hold(ba, osv1alpha1.ReasonBuildFailed, fmt.Sprintf("import the output of imager Job %s/%s: %v", r.Namespace, jobName, err))
			return nil
		}
		logf.FromContext(ctx).Info("imported embedded installer", "bootAsset", ba.Name, "job", jobName, "key", key)
	}

	ba.Status.Digest, ba.Status.Size = sum, size
	ba.Status.ConfigDigest, ba.Status.BuildKey = in.ConfigDigest, key
	setCondition(&ba.Status.Conditions, ba.Generation, osv1alpha1.ConditionReady, metav1.ConditionTrue, osv1alpha1.ReasonArtifactBuilt, "")

	// The build is in; its Job and every older artifact can go.
	if err := r.deleteStaleBuilds(ctx, ba, ""); err != nil {
		return err
	}
	if err := r.Store.Prune(ba.Name, key); err != nil {
		logf.FromContext(ctx).Error(err, "prune superseded installers", "bootAsset", ba.Name)
	}
	return nil
}

// startBuild clears the build's output directory and creates its Job.
// why, when not empty, says why a build that was done once runs again.
func (r *BootAssetReconciler) startBuild(ctx context.Context, ba *osv1alpha1.BootAsset, node *osv1alpha1.TalosNode,
	jobName string, in imagerInputs, why string) error {
	if err := r.Store.PrepareBuild(jobName); err != nil {
		return fmt.Errorf("prepare build %s: %w", jobName, err)
	}
	job := r.Imager.imagerJob(ba, r.Namespace, jobName, node.Status.ConfigSecretRef.Name, in)
	if err := controllerutil.SetControllerReference(ba, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create imager Job %s: %w", jobName, err)
	}
	logf.FromContext(ctx).Info("started imager Job", "bootAsset", ba.Name, "job", jobName, "image", in.Image)
	r.hold(ba, osv1alpha1.ReasonBuilding, fmt.Sprintf("%sstarted imager Job %s/%s", why, r.Namespace, jobName))
	return nil
}

// deleteStaleBuilds removes the asset's build Jobs other than keep (all of
// them when keep is empty). A Job of inputs that moved on would build an
// artifact nobody asks for any more; a finished one has done its work.
func (r *BootAssetReconciler) deleteStaleBuilds(ctx context.Context, ba *osv1alpha1.BootAsset, keep string) error {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(r.Namespace), client.MatchingLabels{labelBootAssetUID: string(ba.UID)}); err != nil {
		return err
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Name == keep || !j.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// sweepStore removes the stored artifacts of BootAssets that are gone, the
// downloads of operations that are gone, and the output of build Jobs that
// are gone. It runs when a BootAsset disappears;
// artifacts outlive their asset only until then.
func (r *BootAssetReconciler) sweepStore(ctx context.Context) error {
	if r.Store == nil {
		return nil
	}
	assets := &osv1alpha1.BootAssetList{}
	if err := r.List(ctx, assets); err != nil {
		return err
	}
	keepAsset := map[string]bool{}
	for _, a := range assets.Items {
		if storeBacked(&a) {
			keepAsset[a.Name] = true
		}
	}
	// The images an operation downloaded by URL stay while it exists; its
	// end removes them, and this removes those of one deleted while the
	// manager was down.
	ops := &metalv1alpha1.HostOperationList{}
	if err := r.List(ctx, ops); err != nil {
		return err
	}
	for _, op := range ops.Items {
		keepAsset[urlStoreName(client.ObjectKeyFromObject(&op))] = true
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(r.Namespace), client.HasLabels{labelBootAssetUID}); err != nil {
		return err
	}
	keepBuild := map[string]bool{}
	for _, j := range jobs.Items {
		keepBuild[j.Name] = true
	}
	return r.Store.Sweep(func(n string) bool { return keepAsset[n] }, func(n string) bool { return keepBuild[n] })
}

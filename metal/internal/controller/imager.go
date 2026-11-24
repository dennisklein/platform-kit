package controller

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/asset"
)

// Imager is how the operator runs the Talos imager to build an installer
// with a node's config embedded. The imager runs as a Job in the operator's
// namespace, writes into the media store volume, and the manager imports the
// result. The image is pinned by digest per Talos release: the imager carries
// the kernel and initramfs of its own release, so the version must match the
// cluster's, and a tag alone could be repointed under a running site.
type Imager struct {
	// Repository is where the imager images are pulled from.
	Repository string
	// Digests pins the imager image of each Talos release (v1.14.6 ->
	// sha256:...). A release without a digest is not built; the BootAsset
	// says which flag adds one.
	Digests map[string]string
	// StoreClaim is the PersistentVolumeClaim of the media store. The Job
	// mounts it; the manager mounts the same claim at its --media-store.
	StoreClaim string
	// HelperImage is the operator's own image. The imager prints the config
	// it embeds, so the Job runs it through a copy of the manager binary that
	// keeps the output out of the log (internal/imagerrun).
	HelperImage string
}

// DefaultImagerRepository is the upstream imager.
const DefaultImagerRepository = "ghcr.io/siderolabs/imager"

// DefaultImagerDigests pins the imager of the Talos release this operator
// renders for (the machinery version in go.mod). The digest is the
// multi-architecture index of ghcr.io/siderolabs/imager:v1.14.6.
func DefaultImagerDigests() map[string]string {
	return map[string]string{
		"v1.14.6": "sha256:726987ad71fc86f098ed77e0fc4cdd594a21f428cefa4970517461e2e631b3b8",
	}
}

// Image is the digest-pinned imager reference for a Talos release.
func (im *Imager) Image(talosVersion string) (string, error) {
	d, ok := im.Digests[talosVersion]
	if !ok {
		known := slices.Sorted(maps.Keys(im.Digests))
		return "", fmt.Errorf("no imager image is pinned for Talos %s (pinned: %v); start the manager with --imager-digest=%s=sha256:<digest of %s:%s>",
			talosVersion, known, talosVersion, im.Repository, talosVersion)
	}
	return im.Repository + ":" + talosVersion + "@" + d, nil
}

// Paths inside the build Job's container.
const (
	imagerStoreMount  = "/store"
	imagerConfigMount = "/config"
	imagerTmp         = "/tmp"
	// imagerHelperDir holds the copy of the manager binary the imager runs
	// through, and imagerBinary is the imager in its own image.
	imagerHelperDir = "/pk-metal"
	imagerBinary    = "/bin/imager"
	// imagerUID is the user the build runs as: the manager's, so that the
	// manager can import and remove what the build wrote.
	imagerUID = int64(65532)
	// labelBootAssetUID marks the build Jobs of one BootAsset. The UID, not
	// the name: a name may be longer than a label value can be.
	labelBootAssetUID = "os.pk.io/boot-asset-uid"
)

// imagerInputs is everything a build depends on; its hash is the build key.
// The config enters as its digest only.
type imagerInputs struct {
	ConfigDigest string   `json:"configDigest"`
	TalosVersion string   `json:"talosVersion"`
	Image        string   `json:"image"`
	KernelArgs   []string `json:"kernelArgs,omitempty"`
	Extensions   []string `json:"extensions,omitempty"`
}

func (in imagerInputs) key() string {
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// imagerKernelArgs are the schematic's kernel arguments minus the one that
// sends Talos looking for a config medium: an embedded config needs none, and
// the medium is not there.
func imagerKernelArgs(s *osv1alpha1.Schematic) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, a := range s.Customization.ExtraKernelArgs {
		if a != bootConfigKernelArg {
			out = append(out, a)
		}
	}
	return out
}

// imagerJobName is build-<asset>-<key>, the asset part cut so the name fits a
// label value (the Job controller labels its pods with it).
func imagerJobName(asset, key string) string {
	const maxAsset = 63 - len("build-") - 1 - 10
	if len(asset) > maxAsset {
		asset = strings.TrimRight(asset[:maxAsset], "-.")
	}
	return "build-" + asset + "-" + key[:10]
}

// imagerArgs is the build container's command line: the manager binary's
// imager-run, then the imager's own command line. The config is read from a
// file on a Secret volume and never appears here: Job arguments are visible
// to anyone who can read the Job.
func imagerArgs(job string, in imagerInputs) []string {
	cfg := path.Join(imagerConfigMount, asset.ConfigFilename)
	args := []string{
		path.Join(imagerHelperDir, "manager"), "imager-run", cfg,
		imagerBinary, "iso",
		"--arch", "amd64",
		"--output", path.Join(imagerStoreMount, asset.BuildDir(job)),
		"--embedded-config-path", cfg,
	}
	for _, a := range in.KernelArgs {
		args = append(args, "--extra-kernel-arg", a)
	}
	for _, e := range in.Extensions {
		args = append(args, "--system-extension-image", e)
	}
	return args
}

// imagerJob is the build Job for one BootAsset and build key.
//
// Least privilege: the imager writes an ISO with xorriso, mtools and
// mksquashfs, all as an ordinary user (no loop devices, no mounts, file
// ownership set with -all-root and cpio -R), so the pod runs under the
// namespace's restricted Pod Security profile: non-root, no capabilities,
// read-only root filesystem, no service account token.
//
// Secrets: the config comes from the node's config Secret as a read-only
// volume. Everything the imager derives from it before the ISO is written
// (the extension tree, the initramfs, the boot image) stays in a
// memory-backed /tmp that ends with the pod. The ISO goes to the media store,
// where the manager imports it.
//
// Logs: the imager prints its whole build profile, the embedded config
// included. An init container copies the manager binary into the pod, and
// the imager runs as its child with its output kept from the log.
//
// The pod is placed next to the manager: the media store is a
// ReadWriteOnce volume, mountable from one node at a time.
func (im *Imager) imagerJob(ba *osv1alpha1.BootAsset, namespace, job, configSecret string, in imagerInputs) *batchv1.Job {
	labels := map[string]string{
		labelBootAssetUID:             string(ba.UID),
		"app.kubernetes.io/name":      "pk-metal",
		"app.kubernetes.io/component": "imager",
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: job, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			// One retry covers a pull that failed halfway; a build that fails
			// twice fails for a reason a person has to read.
			BackoffLimit:          ptr.To(int32(1)),
			ActiveDeadlineSeconds: ptr.To(int64(30 * 60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(imagerUID),
						RunAsGroup:     ptr.To(imagerUID),
						FSGroup:        ptr.To(imagerUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								"app.kubernetes.io/name":      "pk-metal",
								"app.kubernetes.io/component": "manager",
							}},
							TopologyKey: corev1.LabelHostname,
						}},
					}},
					InitContainers: []corev1.Container{{
						Name:            "stage",
						Image:           im.HelperImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/manager", "imager-stage", path.Join(imagerHelperDir, "manager")},
						SecurityContext: restrictedContainer(),
						VolumeMounts:    []corev1.VolumeMount{{Name: "helper", MountPath: imagerHelperDir}},
					}},
					Containers: []corev1.Container{{
						Name:    "imager",
						Image:   in.Image,
						Command: imagerArgs(job, in),
						Env: []corev1.EnvVar{
							{Name: "HOME", Value: imagerTmp},
							{Name: "TMPDIR", Value: imagerTmp},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							// The memory-backed /tmp counts against this.
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")},
						},
						SecurityContext: restrictedContainer(),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "helper", MountPath: imagerHelperDir, ReadOnly: true},
							{Name: "config", MountPath: imagerConfigMount, ReadOnly: true},
							{Name: "store", MountPath: imagerStoreMount},
							{Name: "tmp", MountPath: imagerTmp},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "helper", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: ptr.To(resource.MustParse("256Mi")),
						}}},
						{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName:  configSecret,
							Items:       []corev1.KeyToPath{{Key: asset.ConfigFilename, Path: asset.ConfigFilename}},
							DefaultMode: ptr.To(int32(0o400)),
						}}},
						{Name: "store", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: im.StoreClaim,
						}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: ptr.To(resource.MustParse("2Gi")),
						}}},
					},
				},
			},
		},
	}
}

// restrictedContainer is the container half of the restricted Pod Security
// profile, with a read-only root filesystem on top.
func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// jobFinished reports whether a Job ended, and whether it succeeded, with the
// message of the condition that says so.
func jobFinished(j *batchv1.Job) (done, ok bool, msg string) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true, c.Message
		case batchv1.JobFailed:
			return true, false, cmp.Or(c.Message, c.Reason)
		}
	}
	return false, false, ""
}

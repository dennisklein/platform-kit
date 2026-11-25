package v1alpha1

// AnnotationEtcdSnapshotUploaded on a HostOperation with an etcdRecover step
// records that a person uploaded the snapshot to the node. Its value is
// "<username>: sha256:<hex>": who uploaded, and the sha256 of the plaintext
// that pk-metal restore upload sent. Admission accepts no other form, and
// only the caller's own username (config/policy/guard-lift.yaml). The
// command first checked those bytes against the record of the EtcdSnapshot.
// The step waits for
// it, and only then asks Talos to start etcd from the upload. It is the
// uploader's statement, not proof; Talos checks etcd's own integrity hash
// of the upload when it recovers.
const AnnotationEtcdSnapshotUploaded = "metal.pk.io/etcd-snapshot-uploaded"

// SnapshotUploaded reports whether the value of AnnotationEtcdSnapshotUploaded
// states the upload of sha. A bare sha256 is a value written before
// admission asked for a name; it still counts.
func SnapshotUploaded(value, sha string) bool {
	if value == sha {
		return true
	}
	_, why, named := ParseGuardLift(value)
	return named && why == sha
}

// EtcdRecoverAction restores a cluster's etcd from a snapshot onto one
// control plane, after the cluster lost all of it.
//
// The snapshot is encrypted to people, not to the operator, so the operator
// cannot upload it: after approval a person runs pk-metal restore upload,
// which decrypts it on their machine and streams it to the node with their
// own talosconfig. The step:
//
//  1. refuses unless the cluster is known to have lost its etcd: it was
//     bootstrapped before (status.bootstrappedAt) or is marked
//     os.pk.io/restore-pending, and no control plane runs etcd now;
//  2. waits for the upload (annotation metal.pk.io/etcd-snapshot-uploaded
//     naming the approved sha256);
//  3. asks Talos to bootstrap etcd from the upload (Bootstrap with
//     RecoverEtcd) and waits until the node lists etcd members.
//
// The other control planes then join the restored etcd by themselves, once
// their own etcd data is wiped (a talosReset of EPHEMERAL each).
type EtcdRecoverAction struct {
	// NodeRef names the control-plane TalosNode (os.pk.io) etcd is restored
	// on.
	NodeRef Reference `json:"nodeRef"`

	// SnapshotRef names the EtcdSnapshot (os.pk.io) to restore.
	SnapshotRef Reference `json:"snapshotRef"`

	// SHA256 pins the plaintext snapshot (sha256:<hex>) the approver
	// approved: the EtcdSnapshot's record must still say so, and the upload
	// must name it.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`

	// TimeoutSeconds bounds the step, the wait for the upload included,
	// counted from the previous step's end (or the approval).
	// +kubebuilder:validation:Minimum=300
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=7200
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

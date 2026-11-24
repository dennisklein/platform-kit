package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The etcd backup kinds register themselves here rather than in
// groupversion_info.go's list, so that the kinds other changes add there do
// not collide with these. SchemeBuilder.AddToScheme runs every registered
// function, whenever it was registered.
func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion,
			&EtcdBackupPolicy{}, &EtcdBackupPolicyList{},
			&EtcdSnapshot{}, &EtcdSnapshotList{},
		)
		return nil
	})
}

// Labels the controller sets on every EtcdSnapshot, so that a cluster's or a
// policy's snapshots can be listed with a selector.
const (
	LabelSnapshotCluster = "os.pk.io/cluster"
	LabelSnapshotPolicy  = "os.pk.io/etcd-backup-policy"
)

// AnnotationRestorePending on a TalosCluster says that its etcd is to be
// restored from a snapshot, not started empty: the operator does not
// bootstrap the cluster while it is set, even though it was never seen
// bootstrapped. Set it on a TalosCluster re-created to restore a lost
// cluster (docs site: operate/day-2/restore-etcd, "Majority lost"). Once the
// restored etcd is observed, status.bootstrappedAt takes over and the
// annotation has no further effect.
const AnnotationRestorePending = "os.pk.io/restore-pending"

// EtcdBackupPolicySpec says how often to snapshot one cluster's etcd, how
// long to keep the snapshots, where to keep them and whom to encrypt them for.
type EtcdBackupPolicySpec struct {
	// ClusterRef names the TalosCluster whose etcd is backed up.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is frozen: create another policy for another cluster"
	ClusterRef Reference `json:"clusterRef"`

	// Interval is the time between scheduled snapshots (6h, 30m). At least
	// 5m: a snapshot reads the whole database from one member.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5m')",message="interval must be at least 5m"
	Interval metav1.Duration `json:"interval"`

	// Retention bounds how many snapshots are kept, and for how long.
	// +optional
	Retention EtcdBackupRetention `json:"retention,omitempty"`

	// Sink is where the encrypted snapshots are stored.
	Sink EtcdBackupSink `json:"sink"`

	// Encryption names who can read the snapshots.
	Encryption EtcdBackupEncryption `json:"encryption"`

	// Suspend stops scheduled snapshots; pk-metal backup now still takes
	// one, and retention still prunes.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// EtcdBackupRetention bounds what is kept. Both limits apply. The newest
// verified snapshot is never deleted, whatever its age.
type EtcdBackupRetention struct {
	// Count is how many verified snapshots to keep, the newest.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=14
	// +optional
	Count int32 `json:"count,omitempty"`

	// MaxAge deletes verified snapshots older than this (168h for a week).
	// Unset keeps any age, up to Count.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h')",message="maxAge must be at least 1h"
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
}

// EtcdBackupSink is where snapshots go: exactly one of s3 or pvc.
// +kubebuilder:validation:XValidation:rule="has(self.s3) != has(self.pvc)",message="set exactly one of s3 or pvc"
type EtcdBackupSink struct {
	// S3 stores snapshots in a bucket of an S3-compatible object store,
	// off the cluster: the sink for production.
	// +optional
	S3 *S3Sink `json:"s3,omitempty"`

	// PVC stores snapshots on the operator's backup volume (the
	// config/etcd-backup component mounts a PersistentVolumeClaim into the
	// manager). Refused for the cluster the operator itself runs on: that
	// volume lives on the etcd it would back up, and is lost with it.
	// +optional
	PVC *PVCSink `json:"pvc,omitempty"`
}

// S3Sink is one bucket of an S3-compatible store.
// +kubebuilder:validation:XValidation:rule="self.endpoint.startsWith('https://') || (has(self.allowPlainHTTP) && self.allowPlainHTTP)",message="endpoint must be https://; set allowPlainHTTP: true only for a lab store, because http:// sends the store's credentials in the clear"
type S3Sink struct {
	// Endpoint is the store's URL, https://host[:port]
	// (https://s3.eu-central-1.amazonaws.com, https://minio.example:9000).
	// http:// needs allowPlainHTTP.
	// +kubebuilder:validation:Pattern=`^https?://[^/]+/?$`
	Endpoint string `json:"endpoint"`

	// AllowPlainHTTP lets endpoint be http://, for a store in a lab. The
	// snapshots are encrypted either way, but the store's credentials and
	// the snapshot keys are not, and anyone on the path can delete or
	// replace the ciphertext. Never set it in production.
	// +optional
	AllowPlainHTTP bool `json:"allowPlainHTTP,omitempty"`

	// Bucket must exist; the operator does not create it.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	Bucket string `json:"bucket"`

	// Prefix goes in front of every key; the key is
	// <prefix><cluster>/<snapshot>.db.age.
	// +kubebuilder:validation:Pattern=`^([^/].*/)?$`
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region of the bucket; empty lets the client ask the store.
	// +optional
	Region string `json:"region,omitempty"`

	// PathStyle sends requests as https://host/bucket/key, which most
	// stores other than AWS need.
	// +optional
	PathStyle bool `json:"pathStyle,omitempty"`

	// CredentialsSecretRef names a Secret in the operator's namespace with
	// keys accessKeyID and secretAccessKey. Give the key write, read, list
	// and delete on the bucket (or the prefix) and nothing else.
	CredentialsSecretRef Reference `json:"credentialsSecretRef"`

	// CASecretRef names a Secret in the operator's namespace whose key
	// ca.crt (PEM) replaces the system roots for the endpoint.
	// +optional
	CASecretRef *Reference `json:"caSecretRef,omitempty"`
}

// PVCSink stores snapshots on the operator's backup volume, under
// <cluster>/<snapshot>.db.age. It has no settings: the volume is the one
// mounted into the manager.
type PVCSink struct{}

// EtcdBackupEncryption lists the age public keys snapshots are encrypted to.
type EtcdBackupEncryption struct {
	// AgeRecipients are age public keys (age1..., from age-keygen -y). Any
	// one of their identities decrypts a snapshot; the operator holds none,
	// so it cannot. Name at least two people or a person and a key kept
	// offline, so that a restore does not hang on one key. Post-quantum
	// keys (age1pq1...) cannot be mixed with classic ones.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^age1[0-9a-z]+$`
	// +listType=set
	AgeRecipients []string `json:"ageRecipients"`
}

// SnapshotReference names a snapshot and when it was taken.
type SnapshotReference struct {
	Name    string      `json:"name"`
	TakenAt metav1.Time `json:"takenAt"`
}

// EtcdBackupPolicyStatus is the backup state of the cluster.
type EtcdBackupPolicyStatus struct {
	// Conditions: Ready (the sink and recipients can be used) and BackedUp
	// (a verified snapshot newer than twice the interval exists).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastScheduleTime is when the last scheduled snapshot was started.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulSnapshot is the newest verified snapshot.
	// +optional
	LastSuccessfulSnapshot *SnapshotReference `json:"lastSuccessfulSnapshot,omitempty"`

	// ConsecutiveFailures counts the snapshots that failed since the newest
	// verified one.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// VerifiedSnapshots counts the verified snapshots kept.
	// +optional
	VerifiedSnapshots int32 `json:"verifiedSnapshots,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastSuccessfulSnapshot.takenAt`
// +kubebuilder:printcolumn:name="Failures",type=integer,JSONPath=`.status.consecutiveFailures`
// +kubebuilder:printcolumn:name="BackedUp",type=string,JSONPath=`.status.conditions[?(@.type=="BackedUp")].status`

// EtcdBackupPolicy takes encrypted etcd snapshots of one TalosCluster on a
// schedule and keeps a bounded number of them. Each snapshot is recorded as
// an EtcdSnapshot. Taking a snapshot reads etcd and changes no machine, so
// it needs no approval; restoring one does (docs site: operate/day-2/restore-etcd).
type EtcdBackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EtcdBackupPolicySpec `json:"spec"`
	// +optional
	Status EtcdBackupPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdBackupPolicyList contains a list of EtcdBackupPolicy.
type EtcdBackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdBackupPolicy `json:"items"`
}

// EtcdSnapshotSpec names the policy a snapshot is taken under: its sink and
// recipients.
type EtcdSnapshotSpec struct {
	// PolicyRef names the EtcdBackupPolicy.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="policyRef is frozen"
	PolicyRef Reference `json:"policyRef"`
}

// EtcdSnapshotStatus records one snapshot. The object is the catalogue entry;
// the bytes live in the sink, at Location.
type EtcdSnapshotStatus struct {
	// Conditions: Complete (True once stored; False when taking it failed,
	// which is final: the next schedule takes a new one) and Verified (the
	// plaintext is an intact etcd snapshot and the sink holds exactly the
	// ciphertext sent).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ClusterName is the TalosCluster, copied from the policy when taken.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// TakenAt is when the snapshot was started.
	// +optional
	TakenAt *metav1.Time `json:"takenAt,omitempty"`

	// TakenFrom is the control-plane TalosNode whose etcd member streamed
	// it, and MemberID that member's ID (hex).
	// +optional
	TakenFrom string `json:"takenFrom,omitempty"`
	// +optional
	MemberID string `json:"memberID,omitempty"`

	// RaftIndex is the member's raft index just before the snapshot: the
	// snapshot holds at least everything up to it.
	// +optional
	RaftIndex int64 `json:"raftIndex,omitempty"`

	// Location is where the ciphertext is (s3://bucket/key,
	// pvc://claim/key), and Key its key in the sink.
	// +optional
	Location string `json:"location,omitempty"`
	// +optional
	Key string `json:"key,omitempty"`

	// Size and SHA256 describe the plaintext snapshot (the bbolt database
	// and etcd's 32-byte integrity hash after it): what a restore uploads.
	// pk-metal restore checks the decrypted bytes against SHA256.
	// +optional
	Size int64 `json:"size,omitempty"`
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// EncryptedSize and EncryptedSHA256 describe the object in the sink.
	// +optional
	EncryptedSize int64 `json:"encryptedSize,omitempty"`
	// +optional
	EncryptedSHA256 string `json:"encryptedSHA256,omitempty"`

	// DatabaseTxID is the bbolt transaction id of the database's newest
	// meta page, and PageSize its page size: read off the snapshot as it
	// passed, as evidence that it is a bbolt database.
	// +optional
	DatabaseTxID int64 `json:"databaseTxID,omitempty"`
	// +optional
	PageSize int32 `json:"pageSize,omitempty"`

	// Recipients are the age public keys it was encrypted to.
	// +optional
	// +listType=atomic
	Recipients []string `json:"recipients,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=metal;os
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterName`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.status.takenFrom`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.size`
// +kubebuilder:printcolumn:name="Complete",type=string,JSONPath=`.status.conditions[?(@.type=="Complete")].status`
// +kubebuilder:printcolumn:name="Verified",type=string,JSONPath=`.status.conditions[?(@.type=="Verified")].status`
// +kubebuilder:printcolumn:name="Taken",type=date,JSONPath=`.status.takenAt`

// EtcdSnapshot is one encrypted etcd snapshot of a TalosCluster. The backup
// policy creates one per schedule; pk-metal backup now creates one at once.
// Deleting the object does not delete the stored bytes; retention deletes
// both.
type EtcdSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EtcdSnapshotSpec `json:"spec"`
	// +optional
	Status EtcdSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdSnapshotList contains a list of EtcdSnapshot.
type EtcdSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdSnapshot `json:"items"`
}

// Condition types of the backup kinds.
const (
	// ConditionBackedUp on an EtcdBackupPolicy: a verified snapshot newer
	// than twice the interval exists.
	ConditionBackedUp = "BackedUp"
	// ConditionComplete on an EtcdSnapshot: taking it has ended, stored
	// (True) or failed (False). A TalosRollout uses it too: every node done.
	ConditionComplete = "Complete"
	// ConditionVerified on an EtcdSnapshot: the snapshot is an intact etcd
	// database and the sink holds what was sent.
	ConditionVerified = "Verified"
)

// Reasons of the backup kinds' conditions. Ready uses ReasonSinkReady and the
// refusals below; BackedUp uses ReasonSnapshotRecent and the gaps below.
const (
	ReasonSinkReady = "SinkReady"
	// ReasonSinkRefused: a pvc sink for the cluster the operator runs on.
	ReasonSinkRefused = "SinkRefused"
	// ReasonSinkUnavailable: the sink cannot be used (no backup volume
	// mounted, a credentials Secret missing or incomplete).
	ReasonSinkUnavailable = "SinkUnavailable"
	// ReasonRecipientsInvalid: an age recipient does not parse.
	ReasonRecipientsInvalid = "RecipientsInvalid"

	ReasonSnapshotRecent = "SnapshotRecent"
	// ReasonNoSnapshot: no verified snapshot yet.
	ReasonNoSnapshot = "NoSnapshot"
	// ReasonBackupTooOld: the newest verified snapshot is older than twice
	// the interval.
	ReasonBackupTooOld = "BackupTooOld"

	// ReasonStored on Complete=True; ReasonSnapshotFailed on
	// Complete=False (the message says what failed).
	ReasonStored         = "Stored"
	ReasonSnapshotFailed = "SnapshotFailed"
	// ReasonTaking on Complete=Unknown: the snapshot is being taken, or
	// waits for an etcd member to take it from.
	ReasonTaking = "Taking"
	// ReasonIntact on Verified=True; ReasonDamaged and ReasonReadBackFailed
	// on Verified=False.
	ReasonIntact         = "Intact"
	ReasonDamaged        = "Damaged"
	ReasonReadBackFailed = "ReadBackFailed"
)

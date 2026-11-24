// Package talos is the OS seam: reconcilers and the CLI depend on Client, never
// on the machinery gRPC plumbing. Production dials machineapi; envtest scripts
// the fake, the same discipline as the bmc seam.
//
// The maintenance service is deliberately narrow here: it binds :50000 with no
// client authentication, so on real metal the config rides the BMC's media
// channel. In the operator and the CLI the insecure dial (Target.Insecure)
// only observes: day-0 liveness probing and reading a node in maintenance
// mode. Only the container test tier, which has no media, applies a config
// over it.
package talos

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrAlreadyBootstrapped is what Bootstrap returns when the node's etcd data
// directory is not empty: the cluster was bootstrapped before. Callers treat
// it as success, which is what makes bootstrapping idempotent.
var ErrAlreadyBootstrapped = errors.New("etcd is already bootstrapped")

// Client speaks the Talos machine API to one node.
//
// Every method that changes a machine (apply, bootstrap, the etcd membership
// calls, reboot, shutdown, reset, upgrade) is called only from a step of an
// approved HostOperation, or from the day-0 ladder a person runs; reads
// (members, status, snapshot, a dry-run apply) need no approval.
type Client interface {
	// ApplyConfiguration submits a machine config. The zero ApplyOptions is
	// AUTO mode: the node applies without a reboot when every change can be
	// applied live, and reboots otherwise; the result's Mode says which.
	// With DryRun the node validates the config and
	// reports, in the result's Details, the diff and the mode it would use,
	// and changes nothing.
	ApplyConfiguration(ctx context.Context, machineConfig []byte, opts ApplyOptions) (ApplyResult, error)
	// Bootstrap starts etcd, once per cluster, on one control-plane node. It
	// returns an error wrapping ErrAlreadyBootstrapped when etcd already has
	// data on this node. With RecoverEtcd it starts etcd from the snapshot
	// an earlier EtcdRecover uploaded to this node.
	Bootstrap(ctx context.Context, opts BootstrapOptions) error
	// EtcdMembers lists the etcd members this node's etcd knows. It fails
	// while etcd is not running here, which before bootstrap is always.
	EtcdMembers(ctx context.Context) ([]EtcdMember, error)
	// EtcdStatus reports this node's etcd member: its ID, the leader it
	// follows, the database size and the errors etcd itself reports.
	EtcdStatus(ctx context.Context) (EtcdStatus, error)
	// EtcdAlarms lists the alarms raised in the etcd cluster (NOSPACE,
	// CORRUPT), as this node's etcd sees them.
	EtcdAlarms(ctx context.Context) ([]EtcdAlarm, error)
	// EtcdSnapshot streams a consistent snapshot of this node's etcd
	// database (the bbolt file etcdutl reads) into w, and returns the bytes
	// written. A read: the cluster is not changed.
	EtcdSnapshot(ctx context.Context, w io.Writer) (int64, error)
	// EtcdRecover uploads a snapshot to this node for a later
	// Bootstrap(RecoverEtcd). Talos refuses it on a node whose etcd runs.
	EtcdRecover(ctx context.Context, snapshot io.Reader) error
	// EtcdRemoveMemberByID removes another member from the etcd cluster,
	// asked through this (healthy) node: the path for a member whose node
	// can no longer leave by itself.
	EtcdRemoveMemberByID(ctx context.Context, memberID uint64) error
	// EtcdLeaveCluster makes this node's member leave the etcd cluster, the
	// orderly removal of a control plane that still answers.
	EtcdLeaveCluster(ctx context.Context) error
	// EtcdForfeitLeadership hands leadership to another member when this
	// node leads, so that a reboot or removal of the leader does not cost
	// an election under load. A follower is left as it is.
	EtcdForfeitLeadership(ctx context.Context) error
	// Reboot restarts the node through Talos: services stop, filesystems
	// unmount, then the kernel reboots (or power-cycles, RebootPowerCycle).
	Reboot(ctx context.Context, opts RebootOptions) error
	// Shutdown powers the node off through Talos, gracefully.
	Shutdown(ctx context.Context, opts ShutdownOptions) error
	// Reset wipes the node back towards maintenance mode: with Graceful a
	// control plane leaves etcd and the node is cordoned and drained first.
	// The node answers at once and resets afterwards, so the call returning
	// says only that the request arrived.
	Reset(ctx context.Context, opts ResetOptions) error
	// Upgrade installs another Talos image on the node and reboots into it.
	// With Stage the node writes the image to META and reboots at once; the
	// upgrade runs early in the next boot. It returns the node's
	// acknowledgement.
	Upgrade(ctx context.Context, opts UpgradeOptions) (string, error)
	// BootTime is when the node's kernel last booted. It moves forward
	// exactly when the machine restarted, whatever the path (kexec, warm
	// reboot, power cycle) and however short the outage: the proof a reboot
	// or an upgrade step waits for, where catching the node down would
	// depend on looking at the right moment.
	BootTime(ctx context.Context) (time.Time, error)
	// Version reports the server's version tag. It is served in maintenance
	// mode too, the cheapest liveness probe for a blank machine.
	Version(ctx context.Context) (string, error)
	// Status reports the aggregated machine stage (runtime.MachineStatus),
	// the check-in observable: Stage "running" + Ready is what revokes grants
	// and advances a Host to Provisioned.
	Status(ctx context.Context) (Status, error)
	Close() error
}

// ApplyMode is how a node takes a new config. The values mirror the machine
// API's ApplyConfigurationRequest modes.
type ApplyMode string

const (
	// ApplyAuto lets the node choose: no reboot when every change can be
	// applied live, a reboot otherwise. The zero value.
	ApplyAuto ApplyMode = ""
	// ApplyNoReboot applies live, and fails when a change needs a reboot.
	ApplyNoReboot ApplyMode = "no-reboot"
	// ApplyReboot applies and reboots, whatever changed. Talos deprecates
	// the request mode; it is kept because a person may still ask for it.
	ApplyReboot ApplyMode = "reboot"
	// ApplyStaged writes the config for the next boot and changes nothing
	// now.
	ApplyStaged ApplyMode = "staged"
	// ApplyTry applies live and rolls back after TryTimeout unless a second
	// apply confirms it.
	ApplyTry ApplyMode = "try"
)

// ApplyOptions select the mode of an apply.
type ApplyOptions struct {
	Mode ApplyMode
	// DryRun validates and reports without applying.
	DryRun bool
	// TryTimeout bounds ApplyTry; zero is the node's default (one minute).
	TryTimeout time.Duration
}

// ApplyResult is what the node answered.
type ApplyResult struct {
	// Mode is the mode the node actually chose (AUTO resolves to one).
	Mode ApplyMode
	// Details is the node's human-readable account; on a dry run it carries
	// the config diff and whether a reboot would be needed.
	Details string
	// Warnings are config validation warnings.
	Warnings []string
}

// BootstrapOptions select a fresh bootstrap or a recovery.
type BootstrapOptions struct {
	// RecoverEtcd starts etcd from the snapshot uploaded with EtcdRecover
	// instead of from an empty database.
	RecoverEtcd bool
	// RecoverSkipHashCheck accepts a snapshot without etcd's integrity
	// hash, which is what a copy of the database file (not a snapshot taken
	// over the API) lacks.
	RecoverSkipHashCheck bool
}

// EtcdMember is one member of the etcd cluster.
type EtcdMember struct {
	ID         uint64
	Hostname   string
	PeerURLs   []string
	ClientURLs []string
	// IsLearner: the member receives the log but does not vote yet. Talos
	// adds a joining control plane as a learner and promotes it once it has
	// caught up.
	IsLearner bool
}

// EtcdStatus is one etcd member's own report.
type EtcdStatus struct {
	MemberID uint64
	// LeaderID is the member this one follows (itself when it leads; zero
	// when it knows no leader, which is how a lost quorum looks).
	LeaderID         uint64
	ProtocolVersion  string
	StorageVersion   string
	DBSize           int64
	DBSizeInUse      int64
	RaftIndex        uint64
	RaftTerm         uint64
	RaftAppliedIndex uint64
	IsLearner        bool
	// Errors are the errors etcd itself reports for this member.
	Errors []string
}

// EtcdAlarm is one alarm raised on one member.
type EtcdAlarm struct {
	MemberID uint64
	// Alarm is NONE, NOSPACE or CORRUPT.
	Alarm string
}

// RebootMode is how a reboot ends.
type RebootMode string

const (
	// RebootDefault: the kernel's kexec or reboot path.
	RebootDefault RebootMode = ""
	// RebootPowerCycle: a full power cycle at the end, for firmware that
	// only applies on a cold start.
	RebootPowerCycle RebootMode = "powercycle"
)

// RebootOptions select the reboot mode.
type RebootOptions struct {
	Mode RebootMode
}

// ShutdownOptions select how hard a shutdown pushes.
type ShutdownOptions struct {
	// Force shuts down even when the node cannot cordon and drain itself
	// (the Kubernetes API is down).
	Force bool
}

// WipeMode is which disks a reset wipes.
type WipeMode string

const (
	// WipeAll wipes the system disk and the disks named in
	// ResetOptions.UserDisks. The zero value, as in the machine API.
	WipeAll WipeMode = ""
	// WipeSystemDisk wipes only the system disk; Talos refuses UserDisks
	// with it.
	WipeSystemDisk WipeMode = "system-disk"
	// WipeUserDisks wipes only the disks named in ResetOptions.UserDisks.
	// Talos finds no user disk by itself: with none named it wipes nothing.
	WipeUserDisks WipeMode = "user-disks"
)

// ResetOptions select what a reset does.
type ResetOptions struct {
	// Graceful leaves etcd (on a control plane) and cordons and drains the
	// node before wiping. A node that cannot reach its peers needs false.
	Graceful bool
	// Reboot reboots into maintenance mode afterwards; false powers off.
	Reboot bool
	// Mode picks the disks.
	Mode WipeMode
	// SystemPartitions limits the wipe to these partition labels (STATE,
	// EPHEMERAL, META); empty wipes what Mode names.
	SystemPartitions []string
	// UserDisks are the data disks (device paths, /dev/sdb) to wipe, in
	// mode WipeAll or WipeUserDisks. Talos refuses a path it does not know,
	// a read-only disk and the system disk.
	UserDisks []string
}

// UpgradeRebootMode is how the reboot into a new image ends.
type UpgradeRebootMode string

const (
	UpgradeRebootDefault    UpgradeRebootMode = ""
	UpgradeRebootPowerCycle UpgradeRebootMode = "powercycle"
)

// UpgradeOptions describe one upgrade.
type UpgradeOptions struct {
	// Image is the installer image reference, pinned by digest.
	Image string
	// Stage writes the image to META and reboots now; the upgrade runs
	// early in the next boot, for a node whose open files would otherwise
	// block it. Talos does not cordon or drain the node in this mode.
	Stage bool
	// Preserve keeps the EPHEMERAL partition. Talos 1.8 and later always
	// preserve; the field is passed through for older nodes.
	Preserve bool
	// Force skips the etcd health check Talos runs on a control plane.
	Force      bool
	RebootMode UpgradeRebootMode
}

// Status is the machine's aggregated state.
type Status struct {
	Stage string // unknown | booting | installing | maintenance | running | ...
	Ready bool
	// UnmetConditions name what still blocks readiness at this stage.
	UnmetConditions []string
}

// Target says where and how to dial one node.
type Target struct {
	// Endpoint is the node address (host or host:port; :50000 when bare).
	Endpoint string
	// Insecure dials the maintenance service: encrypted, unauthenticated.
	// Mutually exclusive with Talosconfig.
	Insecure bool
	// Talosconfig is a rendered client config (CA + client cert) for the
	// configured machine.
	Talosconfig []byte
}

// Dialer opens a Client for a target. Reconcilers and the CLI take one, so
// tests can hand them the fake.
type Dialer func(ctx context.Context, t Target) (Client, error)

// With dials t, runs fn on the client and closes it. It returns the dial
// error, or fn's. A failed Close is not reported: every call here is a read
// or an idempotent request, so the connection holds nothing worth keeping.
func With(ctx context.Context, dial Dialer, t Target, fn func(Client) error) error {
	c, err := dial(ctx, t)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

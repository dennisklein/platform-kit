// Package machineapi is the production talos.Client: a thin adapter over the
// machinery gRPC client. Kept free of policy — mode selection (insecure
// maintenance vs talosconfig) is the caller's explicit choice via Target.
package machineapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/dennisklein/platform-kit/metal/internal/talos"
)

// Dial opens a machine-API connection for the target. It is a talos.Dialer.
func Dial(ctx context.Context, t talos.Target) (talos.Client, error) {
	if t.Endpoint == "" {
		return nil, errors.New("machineapi: target endpoint is empty")
	}

	var opts []client.OptionFunc

	switch {
	case t.Insecure && t.Talosconfig != nil:
		return nil, errors.New("machineapi: target sets both Insecure and Talosconfig")
	case t.Insecure:
		// The maintenance service: encrypted, no client auth, self-signed
		// server cert — verification is impossible by construction.
		opts = append(opts,
			client.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // maintenance mode has no verifiable identity
			client.WithEndpoints(t.Endpoint),
		)
	case t.Talosconfig != nil:
		cfg, err := clientconfig.FromBytes(t.Talosconfig)
		if err != nil {
			return nil, fmt.Errorf("machineapi: parse talosconfig: %w", err)
		}

		opts = append(opts, client.WithConfig(cfg), client.WithEndpoints(t.Endpoint))
	default:
		return nil, errors.New("machineapi: target needs Insecure or a Talosconfig")
	}

	c, err := client.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("machineapi: dial %s: %w", t.Endpoint, err)
	}

	return &conn{c: c}, nil
}

// CallTimeout bounds every machine-API call. A controller's context has no
// deadline, and a node that is up but not ready (etcd not started, apid still
// waiting for its peers) can hold an unbounded gRPC call open, which stops the
// controller's only worker for good.
var CallTimeout = 20 * time.Second

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CallTimeout)
}

type conn struct {
	c *client.Client
}

// LongCallTimeout bounds the calls that move a whole database or image: a
// snapshot or recovery upload of a multi-gigabyte etcd, and an upgrade, which
// the node acknowledges only after it pulled and checked the installer image.
var LongCallTimeout = 30 * time.Minute

func boundedLong(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, LongCallTimeout)
}

var applyModes = map[talos.ApplyMode]machineapi.ApplyConfigurationRequest_Mode{
	talos.ApplyAuto:     machineapi.ApplyConfigurationRequest_AUTO,
	talos.ApplyNoReboot: machineapi.ApplyConfigurationRequest_NO_REBOOT,
	talos.ApplyReboot:   machineapi.ApplyConfigurationRequest_REBOOT, //nolint:staticcheck // talosctl --mode=reboot still sends it; Talos 1.14 serves it
	talos.ApplyStaged:   machineapi.ApplyConfigurationRequest_STAGED,
	talos.ApplyTry:      machineapi.ApplyConfigurationRequest_TRY,
}

func applyModeOf(m machineapi.ApplyConfigurationRequest_Mode) talos.ApplyMode {
	for k, v := range applyModes {
		if v == m {
			return k
		}
	}
	return talos.ApplyMode(m.String())
}

func (m *conn) ApplyConfiguration(ctx context.Context, machineConfig []byte, opts talos.ApplyOptions) (talos.ApplyResult, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	mode, ok := applyModes[opts.Mode]
	if !ok {
		return talos.ApplyResult{}, fmt.Errorf("apply configuration: unknown mode %q", opts.Mode)
	}
	req := &machineapi.ApplyConfigurationRequest{Data: machineConfig, Mode: mode, DryRun: opts.DryRun}
	if opts.TryTimeout > 0 {
		req.TryModeTimeout = durationpb.New(opts.TryTimeout)
	}

	resp, err := m.c.ApplyConfiguration(ctx, req)
	if err != nil {
		return talos.ApplyResult{}, fmt.Errorf("apply configuration: %w", err)
	}

	// One node is dialed, so there is one message; a missing one is an
	// older server's empty answer, which still means accepted.
	var res talos.ApplyResult
	if msgs := resp.GetMessages(); len(msgs) > 0 {
		res = talos.ApplyResult{
			Mode:     applyModeOf(msgs[0].GetMode()),
			Details:  msgs[0].GetModeDetails(),
			Warnings: msgs[0].GetWarnings(),
		}
	}

	return res, nil
}

func (m *conn) Bootstrap(ctx context.Context, opts talos.BootstrapOptions) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if err := m.c.Bootstrap(ctx, &machineapi.BootstrapRequest{
		RecoverEtcd:          opts.RecoverEtcd,
		RecoverSkipHashCheck: opts.RecoverSkipHashCheck,
	}); err != nil {
		// Talos answers AlreadyExists exactly when the etcd data directory
		// is not empty, which is the observable form of "bootstrapped".
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("bootstrap: %w: %w", talos.ErrAlreadyBootstrapped, err)
		}

		return fmt.Errorf("bootstrap: %w", err)
	}

	return nil
}

func (m *conn) EtcdMembers(ctx context.Context) ([]talos.EtcdMember, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{QueryLocal: true})
	if err != nil {
		return nil, fmt.Errorf("etcd members: %w", err)
	}

	var members []talos.EtcdMember

	for _, msg := range resp.GetMessages() {
		for _, member := range msg.GetMembers() {
			members = append(members, talos.EtcdMember{
				ID:         member.GetId(),
				Hostname:   member.GetHostname(),
				PeerURLs:   member.GetPeerUrls(),
				ClientURLs: member.GetClientUrls(),
				IsLearner:  member.GetIsLearner(),
			})
		}
	}

	return members, nil
}

func (m *conn) EtcdStatus(ctx context.Context) (talos.EtcdStatus, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.EtcdStatus(ctx)
	if err != nil {
		return talos.EtcdStatus{}, fmt.Errorf("etcd status: %w", err)
	}

	if len(resp.GetMessages()) == 0 || resp.GetMessages()[0].GetMemberStatus() == nil {
		return talos.EtcdStatus{}, errors.New("etcd status: empty response")
	}

	st := resp.GetMessages()[0].GetMemberStatus()

	return talos.EtcdStatus{
		MemberID:         st.GetMemberId(),
		LeaderID:         st.GetLeader(),
		ProtocolVersion:  st.GetProtocolVersion(),
		StorageVersion:   st.GetStorageVersion(),
		DBSize:           st.GetDbSize(),
		DBSizeInUse:      st.GetDbSizeInUse(),
		RaftIndex:        st.GetRaftIndex(),
		RaftTerm:         st.GetRaftTerm(),
		RaftAppliedIndex: st.GetRaftAppliedIndex(),
		IsLearner:        st.GetIsLearner(),
		Errors:           st.GetErrors(),
	}, nil
}

func (m *conn) EtcdAlarms(ctx context.Context) ([]talos.EtcdAlarm, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.EtcdAlarmList(ctx)
	if err != nil {
		return nil, fmt.Errorf("etcd alarms: %w", err)
	}

	var alarms []talos.EtcdAlarm

	for _, msg := range resp.GetMessages() {
		for _, a := range msg.GetMemberAlarms() {
			alarms = append(alarms, talos.EtcdAlarm{MemberID: a.GetMemberId(), Alarm: a.GetAlarm().String()})
		}
	}

	return alarms, nil
}

func (m *conn) EtcdSnapshot(ctx context.Context, w io.Writer) (int64, error) {
	ctx, cancel := boundedLong(ctx)
	defer cancel()

	rc, err := m.c.EtcdSnapshot(ctx, &machineapi.EtcdSnapshotRequest{})
	if err != nil {
		return 0, fmt.Errorf("etcd snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// ReadStream reports a server-side failure as the read error, so a
	// snapshot cut short never reads as a complete one.
	n, err := io.Copy(w, rc)
	if err != nil {
		return n, fmt.Errorf("etcd snapshot: after %d bytes: %w", n, err)
	}

	return n, nil
}

func (m *conn) EtcdRecover(ctx context.Context, snapshot io.Reader) error {
	ctx, cancel := boundedLong(ctx)
	defer cancel()

	if _, err := m.c.EtcdRecover(ctx, snapshot); err != nil {
		return fmt.Errorf("etcd recover: %w", err)
	}

	return nil
}

func (m *conn) EtcdRemoveMemberByID(ctx context.Context, memberID uint64) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if err := m.c.EtcdRemoveMemberByID(ctx, &machineapi.EtcdRemoveMemberByIDRequest{MemberId: memberID}); err != nil {
		return fmt.Errorf("etcd remove member %x: %w", memberID, err)
	}

	return nil
}

func (m *conn) EtcdLeaveCluster(ctx context.Context) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if err := m.c.EtcdLeaveCluster(ctx, &machineapi.EtcdLeaveClusterRequest{}); err != nil {
		return fmt.Errorf("etcd leave cluster: %w", err)
	}

	return nil
}

func (m *conn) EtcdForfeitLeadership(ctx context.Context) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if _, err := m.c.EtcdForfeitLeadership(ctx, &machineapi.EtcdForfeitLeadershipRequest{}); err != nil {
		return fmt.Errorf("etcd forfeit leadership: %w", err)
	}

	return nil
}

func (m *conn) Reboot(ctx context.Context, opts talos.RebootOptions) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	var mode machineapi.RebootRequest_Mode

	switch opts.Mode {
	case talos.RebootDefault:
		mode = machineapi.RebootRequest_DEFAULT
	case talos.RebootPowerCycle:
		mode = machineapi.RebootRequest_POWERCYCLE
	default:
		return fmt.Errorf("reboot: unknown mode %q", opts.Mode)
	}

	if err := m.c.Reboot(ctx, client.WithRebootMode(mode)); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}

	return nil
}

func (m *conn) Shutdown(ctx context.Context, opts talos.ShutdownOptions) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if err := m.c.Shutdown(ctx, client.WithShutdownForce(opts.Force)); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}

func (m *conn) Reset(ctx context.Context, opts talos.ResetOptions) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	req := &machineapi.ResetRequest{
		Graceful:        opts.Graceful,
		Reboot:          opts.Reboot,
		UserDisksToWipe: opts.UserDisks,
	}

	switch opts.Mode {
	case talos.WipeAll:
		req.Mode = machineapi.ResetRequest_ALL
	case talos.WipeSystemDisk:
		req.Mode = machineapi.ResetRequest_SYSTEM_DISK
	case talos.WipeUserDisks:
		req.Mode = machineapi.ResetRequest_USER_DISKS
	default:
		return fmt.Errorf("reset: unknown wipe mode %q", opts.Mode)
	}

	for _, label := range opts.SystemPartitions {
		req.SystemPartitionsToWipe = append(req.SystemPartitionsToWipe, &machineapi.ResetPartitionSpec{Label: label, Wipe: true})
	}

	if err := m.c.ResetGeneric(ctx, req); err != nil {
		return fmt.Errorf("reset: %w", err)
	}

	return nil
}

func (m *conn) Upgrade(ctx context.Context, opts talos.UpgradeOptions) (string, error) {
	ctx, cancel := boundedLong(ctx)
	defer cancel()

	if opts.Image == "" {
		return "", errors.New("upgrade: no image")
	}

	var rebootMode machineapi.UpgradeRequest_RebootMode

	switch opts.RebootMode {
	case talos.UpgradeRebootDefault:
		rebootMode = machineapi.UpgradeRequest_DEFAULT
	case talos.UpgradeRebootPowerCycle:
		rebootMode = machineapi.UpgradeRequest_POWERCYCLE
	default:
		return "", fmt.Errorf("upgrade: unknown reboot mode %q", opts.RebootMode)
	}

	// MachineService.Upgrade is deprecated in favour of LifecycleService,
	// whose Upgrade only installs to disk: it has no stage, force or reboot
	// mode, so the caller would sequence the reboot itself. Talos 1.14 still
	// serves the machine-service call, which does it all in one request.
	// The API marks it for removal in Talos v1.18: by then the upgrade step
	// moves to LifecycleService.Upgrade followed by its own Reboot.
	resp, err := m.c.UpgradeWithOptions(ctx, //nolint:staticcheck // see above: LifecycleService lacks stage/reboot
		client.WithUpgradeImage(opts.Image),
		client.WithUpgradeStage(opts.Stage),
		client.WithUpgradePreserve(opts.Preserve),
		client.WithUpgradeForce(opts.Force),
		client.WithUpgradeRebootMode(rebootMode),
	)
	if err != nil {
		return "", fmt.Errorf("upgrade: %w", err)
	}

	if len(resp.GetMessages()) == 0 {
		return "", nil
	}

	return resp.GetMessages()[0].GetAck(), nil
}

func (m *conn) BootTime(ctx context.Context) (time.Time, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.MachineClient.SystemStat(ctx, &emptypb.Empty{})
	if err != nil {
		return time.Time{}, fmt.Errorf("system stat: %w", err)
	}

	msgs := resp.GetMessages()
	if len(msgs) == 0 || msgs[0].GetBootTime() == 0 {
		return time.Time{}, errors.New("system stat: no boot time in the answer")
	}

	return time.Unix(int64(msgs[0].GetBootTime()), 0), nil //nolint:gosec // seconds since the epoch fit an int64
}

func (m *conn) Version(ctx context.Context) (string, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}

	if len(resp.GetMessages()) == 0 || resp.GetMessages()[0].GetVersion() == nil {
		return "", errors.New("version: empty response")
	}

	return resp.GetMessages()[0].GetVersion().GetTag(), nil
}

func (m *conn) Status(ctx context.Context) (talos.Status, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	ms, err := safe.StateGetByID[*runtimeres.MachineStatus](ctx, m.c.COSI, runtimeres.MachineStatusID)
	if err != nil {
		return talos.Status{}, fmt.Errorf("machine status: %w", err)
	}

	spec := ms.TypedSpec()

	unmet := make([]string, 0, len(spec.Status.UnmetConditions))
	for _, c := range spec.Status.UnmetConditions {
		unmet = append(unmet, c.Name)
	}

	return talos.Status{
		Stage:           spec.Stage.String(),
		Ready:           spec.Status.Ready,
		UnmetConditions: unmet,
	}, nil
}

func (m *conn) Close() error {
	return m.c.Close()
}

// Package machineapi is the production talos.Client: a thin adapter over the
// machinery gRPC client. Kept free of policy — mode selection (insecure
// maintenance vs talosconfig) is the caller's explicit choice via Target.
package machineapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	runtimeres "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func (m *conn) ApplyConfiguration(ctx context.Context, machineConfig []byte) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	_, err := m.c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: machineConfig,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	})
	if err != nil {
		return fmt.Errorf("apply configuration: %w", err)
	}

	return nil
}

func (m *conn) Bootstrap(ctx context.Context) error {
	ctx, cancel := bounded(ctx)
	defer cancel()

	if err := m.c.Bootstrap(ctx, &machineapi.BootstrapRequest{}); err != nil {
		// Talos answers AlreadyExists exactly when the etcd data directory
		// is not empty, which is the observable form of "bootstrapped".
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("bootstrap: %w: %w", talos.ErrAlreadyBootstrapped, err)
		}

		return fmt.Errorf("bootstrap: %w", err)
	}

	return nil
}

func (m *conn) EtcdMembers(ctx context.Context) ([]string, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	resp, err := m.c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{QueryLocal: true})
	if err != nil {
		return nil, fmt.Errorf("etcd members: %w", err)
	}

	var names []string

	for _, msg := range resp.GetMessages() {
		for _, member := range msg.GetMembers() {
			names = append(names, member.GetHostname())
		}
	}

	return names, nil
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

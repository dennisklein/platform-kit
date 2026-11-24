// Package docker runs a Talos container as the machine behind a simulated BMC —
// the e2e tier's machine backend. The container shape is the spike-proven one:
// no config injected, so the machine boots blank into maintenance mode and the
// provisioning ladder delivers the config exactly as it would reach metal
// (modulo the media channel — a container has none, which is this tier's stated
// fidelity boundary).
//
// Talos ≥1.13 offers no talosctl path to a blank node (`cluster create` injects
// its own config at creation), so the harness runs the container itself, via
// the docker CLI — the one tool every dev/CI host of this tier already has.
package docker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Machine is one Talos container. Zero value is not usable — New fills the
// invariants.
type Machine struct {
	name    string
	image   string
	network string
	ip      string
}

// New names a machine. Nothing is created until Ensure.
//   - name: the container name (one per simulated system)
//   - image: the Talos release image, e.g. ghcr.io/siderolabs/talos:v1.13.4
//   - network: the docker network to join; empty joins the default bridge
//   - ip: a static address on that network (user-defined networks only; empty
//     lets docker assign one) — what makes a fixture's ipAddress deterministic
func New(name, image, network, ip string) *Machine {
	return &Machine{name: name, image: image, network: network, ip: ip}
}

// Ensure creates the container, stopped and blank, if it does not exist. The
// mounts and flags mirror hack/talos-docker-spike.sh — the recorded experiment
// this driver encodes.
func (m *Machine) Ensure(ctx context.Context) error {
	if _, err := m.docker(ctx, "container", "inspect", m.name); err == nil {
		return nil
	}
	args := []string{
		"create", "--name", m.name, "--hostname", m.name,
		"--read-only", "--privileged", "--security-opt", "seccomp=unconfined",
		"--mount", "type=tmpfs,destination=/run",
		"--mount", "type=tmpfs,destination=/system",
		"--mount", "type=tmpfs,destination=/tmp",
		"--mount", "type=volume,destination=/system/state",
		"--mount", "type=volume,destination=/var",
		"--mount", "type=volume,destination=/etc/cni",
		"--mount", "type=volume,destination=/etc/kubernetes",
		"--mount", "type=volume,destination=/usr/libexec/kubernetes",
		"--mount", "type=volume,destination=/opt",
		"-e", "PLATFORM=container",
	}
	if m.network != "" {
		args = append(args, "--network", m.network)
	}
	if m.ip != "" {
		args = append(args, "--ip", m.ip)
	}
	args = append(args, m.image)
	_, err := m.docker(ctx, args...)
	return err
}

func (m *Machine) Start(ctx context.Context) error {
	_, err := m.docker(ctx, "start", m.name)
	return err
}

func (m *Machine) Stop(ctx context.Context) error {
	_, err := m.docker(ctx, "stop", "--time", "2", m.name)
	return err
}

func (m *Machine) Restart(ctx context.Context) error {
	_, err := m.docker(ctx, "restart", "--time", "2", m.name)
	return err
}

func (m *Machine) Running(ctx context.Context) (bool, error) {
	out, err := m.docker(ctx, "inspect", "-f", "{{.State.Running}}", m.name)
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

// IP reports the container's address on its network — what the harness writes
// into TalosNode.spec.ipAddress. Empty until the container runs.
func (m *Machine) IP(ctx context.Context) (string, error) {
	format := "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"
	return m.docker(ctx, "inspect", "-f", format, m.name)
}

// Remove force-removes the container and its anonymous volumes — the machine
// ceases to exist, disks and all (the next Ensure births a blank one).
func (m *Machine) Remove(ctx context.Context) error {
	_, err := m.docker(ctx, "rm", "-f", "-v", m.name)
	return err
}

func (m *Machine) docker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

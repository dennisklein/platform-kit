package controller

import (
	"context"
	"fmt"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/oci"
)

// stageFirmware stages a system firmware image for each target entry that
// does not already report the wanted version. The goal of the step is
// "staged or already there": staging changes nothing that runs, the reboot
// later in the operation applies it, and the convergence lane reads the
// version back afterwards. A resumed pass stages again what it staged
// before, which leaves the same image waiting for the same reset.
func (x *exec) stageFirmware(ctx context.Context, a *metalv1alpha1.StageFirmwareAction) error {
	cl, _, err := x.client(ctx)
	if err != nil {
		return err
	}
	inv, err := cl.FirmwareInventory(ctx)
	if err != nil {
		return err
	}
	versions := map[string]string{}
	for _, e := range inv {
		versions[e.ID] = e.Version
	}
	var image []byte
	for _, target := range a.Targets {
		v, ok := versions[target]
		if !ok {
			return fmt.Errorf("the BMC's firmware inventory has no entry %s", target)
		}
		if v == a.Version {
			continue // goal already met
		}
		if image == nil {
			if image, err = oci.Pull(ctx, x.r.RegistryClient, a.Image); err != nil {
				return err
			}
		}
		if err := cl.StageFirmware(ctx, image, target); err != nil {
			return fmt.Errorf("stage %s %s for %s: %w", a.Image, a.Version, target, err)
		}
	}
	return nil
}

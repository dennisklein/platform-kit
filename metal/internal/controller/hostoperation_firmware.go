package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/oci"
)

// stageFirmware stages a system firmware image for each target entry that
// does not already report the wanted version. The goal of the step is
// "staged or already there": staging changes nothing that runs, the reboot
// later in the operation applies it, and the convergence lane reads the
// version back afterwards. Each target is recorded before its image is
// sent, so a resumed pass does not send it a second time.
func (x *exec) stageFirmware(ctx context.Context, i int, a *metalv1alpha1.StageFirmwareAction) error {
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
	sent := x.upload(i)
	for _, target := range a.Targets {
		v, ok := versions[target]
		if !ok {
			return fmt.Errorf("the BMC's firmware inventory has no entry %s", target)
		}
		if v == a.Version {
			continue // goal already met
		}
		if sent != nil && slices.Contains(sent.Targets, target) {
			continue // sent before this pass
		}
		if image == nil {
			if image, err = oci.Pull(ctx, x.r.RegistryClient, a.Image); err != nil {
				return err
			}
		}
		if sent == nil {
			sent = &metalv1alpha1.FirmwareUploadProgress{Step: int32(i), SentAt: metav1.MicroTime{Time: time.Now()}}
		}
		sent.Targets = append(sent.Targets, target)
		if err := x.recordUpload(sent); err != nil {
			return err
		}
		if err := cl.StageFirmware(ctx, image, target); err != nil {
			return fmt.Errorf("stage %s %s for %s: %w", a.Image, a.Version, target, err)
		}
	}
	return nil
}

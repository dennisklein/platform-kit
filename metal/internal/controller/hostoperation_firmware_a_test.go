package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metalv1alpha1 "github.com/dennisklein/platform-kit/metal/api/v1alpha1"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// firmwareRegistry serves one single-layer artifact over TLS and returns
// the client that trusts it and the artifact's reference.
func firmwareRegistry(t *testing.T, blob []byte) (*http.Client, string) {
	t.Helper()
	blobDigest := digest.FromBytes(blob).String()
	man := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"layers":[{"mediaType":"application/octet-stream","digest":%q,"size":%d}]}`, blobDigest, len(blob))
	manDigest := digest.FromBytes([]byte(man)).String()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/firmware/bmc/manifests/"+manDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(man))
	})
	mux.HandleFunc("GET /v2/firmware/bmc/blobs/"+blobDigest, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(blob)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv.Client(), strings.TrimPrefix(srv.URL, "https://") + "/firmware/bmc@" + manDigest
}

// flashingBMC is a manager that takes a firmware upload and reports the new
// version only once flashed is set. It counts the uploads and stages.
type flashingBMC struct {
	bmc.Client
	mu       sync.Mutex
	uploads  int
	staged   []string
	flashed  bool
	onUpload func()
	reset    time.Time
	versions map[string]string
}

func (b *flashingBMC) Manager(context.Context) (bmc.Manager, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.flashed {
		return bmc.Manager{FirmwareVersion: "2.0", LastResetTime: b.reset.Add(time.Minute)}, nil
	}
	return bmc.Manager{FirmwareVersion: "1.0", LastResetTime: b.reset}, nil
}

func (b *flashingBMC) UpdateFirmware(context.Context, []byte) error {
	b.mu.Lock()
	b.uploads++
	f := b.onUpload
	b.mu.Unlock()
	if f != nil {
		f()
	}
	return nil
}

func (b *flashingBMC) FirmwareInventory(context.Context) ([]bmc.Firmware, error) {
	var out []bmc.Firmware
	for id, v := range b.versions {
		out = append(out, bmc.Firmware{ID: id, Version: v, Updateable: true})
	}
	return out, nil
}

func (b *flashingBMC) StageFirmware(_ context.Context, _ []byte, entry string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.staged = append(b.staged, entry)
	if b.onUpload != nil {
		b.onUpload()
	}
	return nil
}

// A pass that ends after the flash upload (the operator restarts, the
// leader changes) leaves a record. The resumed pass only waits for the
// version: the image goes to the manager once.
func TestResumedFlashDoesNotUploadAgain(t *testing.T) {
	hc, ref := firmwareRegistry(t, []byte("BMC firmware 2.0"))
	r := &HostOperationReconciler{RegistryClient: hc}
	op := &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{Actions: []metalv1alpha1.HostOperationAction{
		{FlashFirmware: &metalv1alpha1.FlashFirmwareAction{Image: ref, Version: "2.0"}},
	}}}
	fake := &flashingBMC{reset: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	flushes := 0

	// The first pass ends right after the upload.
	ctx, cancel := context.WithCancel(t.Context())
	fake.onUpload = cancel
	x := &exec{r: r, op: op, cl: fake, systemID: "1", flush: func() error { flushes++; return nil }}
	require.Error(t, x.flashFirmware(ctx, 0, fake, op.Spec.Actions[0].FlashFirmware))
	require.NotNil(t, op.Status.UploadAccepted, "the upload is recorded")
	assert.Equal(t, 1, flushes, "and persisted before it is sent")
	require.NotNil(t, op.Status.UploadAccepted.ManagerResetTime)

	// The resumed pass: the manager finishes its flash meanwhile.
	fake.onUpload = nil
	fake.flashed = true
	x = &exec{r: r, op: op, cl: fake, systemID: "1", flush: func() error { flushes++; return nil }}
	require.NoError(t, x.flashFirmware(t.Context(), 0, fake, op.Spec.Actions[0].FlashFirmware))
	assert.Equal(t, 1, fake.uploads, "one upload over both passes")
}

// A resumed stage step does not send the image again for a target it sent
// it for before.
func TestResumedStageDoesNotUploadAgain(t *testing.T) {
	hc, ref := firmwareRegistry(t, []byte("NIC firmware 9"))
	r := &HostOperationReconciler{RegistryClient: hc}
	op := &metalv1alpha1.HostOperation{Spec: metalv1alpha1.HostOperationSpec{Actions: []metalv1alpha1.HostOperationAction{
		{StageFirmware: &metalv1alpha1.StageFirmwareAction{Image: ref, Version: "9", Targets: []string{"NIC.1", "NIC.2"}}},
	}}}
	fake := &flashingBMC{versions: map[string]string{"NIC.1": "8", "NIC.2": "8"}}
	ctx, cancel := context.WithCancel(t.Context())
	fake.onUpload = cancel
	x := &exec{r: r, op: op, cl: fake, systemID: "1", flush: func() error { return nil }}
	// The first pass ends after the first target's upload.
	_ = x.stageFirmware(ctx, 0, op.Spec.Actions[0].StageFirmware)
	require.NotNil(t, op.Status.UploadAccepted)

	fake.onUpload = nil
	x = &exec{r: r, op: op, cl: fake, systemID: "1", flush: func() error { return nil }}
	require.NoError(t, x.stageFirmware(t.Context(), 0, op.Spec.Actions[0].StageFirmware))
	assert.Equal(t, []string{"NIC.1", "NIC.2"}, fake.staged, "each target once")
}

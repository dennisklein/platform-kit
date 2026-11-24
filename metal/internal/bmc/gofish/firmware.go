package gofish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
)

// smcInstallAction is the X12's OEM install action. An UpdateService that
// advertises it is one whose multipart push needs the Supermicro form.
const smcInstallAction = "#SmcUpdateService.Install"

// updateService is the raw UpdateService: the push URI, the apply times it
// supports, and the actions gofish keeps private.
type updateService struct {
	ODataID   string `json:"@odata.id"`
	PushURI   string `json:"MultipartHttpPushUri"`
	ApplyTime struct {
		SupportedValues []string `json:"SupportedValues"`
	} `json:"MultipartHttpPushUri@Redfish.OperationApplyTimeSupport"`
	Actions struct {
		SimpleUpdate struct {
			Target string `json:"target"`
		} `json:"#UpdateService.SimpleUpdate"`
	} `json:"Actions"`
	oem   map[string]string
	typed *schemas.UpdateService
}

// updateService reads the UpdateService once per session: its push URI,
// apply times and actions do not change while the session lasts.
func (c *client) updateService() (*updateService, error) {
	c.memo.Lock()
	cached := c.memo.update
	c.memo.Unlock()
	if cached != nil {
		return cached, nil
	}
	us, err := c.svc.UpdateService()
	if err != nil {
		return nil, fmt.Errorf("bmc: update service: %w", err)
	}
	if us == nil || us.ODataID == "" {
		return nil, fmt.Errorf("bmc: endpoint exposes no update service")
	}
	doc := updateService{typed: us}
	if err := json.Unmarshal(us.RawData, &doc); err != nil {
		return nil, fmt.Errorf("bmc: update service: %w", err)
	}
	doc.oem = oemActions(us.RawData)
	c.memo.Lock()
	c.memo.update = &doc
	c.memo.Unlock()
	return &doc, nil
}

// updateDialect probes the UpdateService: the push URI wins, SimpleUpdate is
// noted where it's all there is (advertised ≠ proven — see bmc.DialectSimpleUpdate).
func (c *client) updateDialect() bmc.UpdateDialect {
	us, err := c.updateService()
	if err != nil {
		return bmc.DialectNoUpdate
	}
	switch {
	case us.PushURI != "":
		return bmc.DialectMultipartPush
	case us.Actions.SimpleUpdate.Target != "":
		return bmc.DialectSimpleUpdate
	default:
		return bmc.DialectNoUpdate
	}
}

func (c *client) FirmwareInventory(ctx context.Context) ([]bmc.Firmware, error) {
	us, err := c.updateService()
	if err != nil {
		return nil, err
	}
	inv, err := us.typed.FirmwareInventory()
	if err != nil {
		return nil, fmt.Errorf("bmc: firmware inventory: %w", err)
	}
	out := make([]bmc.Firmware, 0, len(inv))
	for _, e := range inv {
		out = append(out, bmc.Firmware{ID: e.ID, Version: e.Version, Updateable: e.Updateable})
	}
	// gofish walks collections concurrently — sort for stable output.
	slices.SortFunc(out, func(a, b bmc.Firmware) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// UpdateFirmware pushes a manager image over the advertised multipart push
// URI: one POST, the UpdateParameters part typed application/json and first,
// the UpdateFile part second (gofish's PostMultipart guarantees neither order
// nor content types, so the body is built by hand).
//
// The parameters come in two forms. The standard one targets the manager's
// active firmware image and asks for immediate application. An UpdateService
// that advertises the Supermicro install action gets the form proven on the
// X12 instead: the manager itself as the target and the Oem.Supermicro.BMC
// preserve block, without which that firmware stages and never installs.
//
// The upload answers with a task monitor, which is followed to its end. A
// monitor that stops answering is not a failure here — flashing reboots the
// manager, and its tasks with it. Acceptance, even a completed task, is still
// not proof: callers judge by the version and LastResetTime reading back.
func (c *client) UpdateFirmware(ctx context.Context, image []byte) error {
	if c.readOnly {
		return errReadOnly
	}
	us, err := c.updateService()
	if err != nil {
		return err
	}
	if us.PushURI == "" {
		return fmt.Errorf("bmc: endpoint advertises no multipart push URI")
	}
	params, err := c.updateParameters(us)
	if err != nil {
		return err
	}
	body, contentType, err := updateForm(params, image)
	if err != nil {
		return err
	}
	resp, err := c.api.RunRawRequestWithHeaders(http.MethodPost, us.PushURI,
		bytes.NewReader(body), contentType, nil)
	if err != nil {
		return fmt.Errorf("bmc: firmware upload: %w", err)
	}
	defer resp.Body.Close()
	if err := c.pollAccepted(ctx, resp); err != nil && !errors.Is(err, errTaskLost) {
		return fmt.Errorf("bmc: firmware update: %w", err)
	}
	return nil
}

// StageFirmware pushes a system component's image over the multipart push
// URI with apply time OnReset and the inventory entry as the target. The
// answer is not followed to its end: a task for a staged update stays
// pending until the system resets, which is a later step's business.
func (c *client) StageFirmware(_ context.Context, image []byte, entryID string) error {
	if c.readOnly {
		return errReadOnly
	}
	us, err := c.updateService()
	if err != nil {
		return err
	}
	if us.PushURI == "" {
		return fmt.Errorf("bmc: endpoint advertises no multipart push URI")
	}
	if !slices.Contains(us.ApplyTime.SupportedValues, "OnReset") {
		return fmt.Errorf("bmc: the push URI does not support apply time OnReset (it lists %v); system firmware is only staged for the next reset",
			us.ApplyTime.SupportedValues)
	}
	inv, err := c.svc.UpdateService()
	if err != nil {
		return fmt.Errorf("bmc: update service: %w", err)
	}
	entries, err := inv.FirmwareInventory()
	if err != nil {
		return fmt.Errorf("bmc: firmware inventory: %w", err)
	}
	target := ""
	for _, e := range entries {
		if e.ID == entryID {
			target = e.ODataID
		}
	}
	if target == "" {
		return fmt.Errorf("bmc: firmware inventory has no entry %q", entryID)
	}
	params, err := json.Marshal(map[string]any{
		"Targets":                     []string{target},
		"@Redfish.OperationApplyTime": "OnReset",
	})
	if err != nil {
		return err
	}
	body, contentType, err := updateForm(params, image)
	if err != nil {
		return err
	}
	resp, err := c.api.RunRawRequestWithHeaders(http.MethodPost, us.PushURI,
		bytes.NewReader(body), contentType, nil)
	if err != nil {
		return fmt.Errorf("bmc: firmware upload: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// updateParameters builds the UpdateParameters part for this endpoint.
func (c *client) updateParameters(us *updateService) ([]byte, error) {
	mgr, err := c.managerURI()
	if err != nil {
		return nil, err
	}
	if us.oem[smcInstallAction] != "" {
		return json.Marshal(map[string]any{
			"Targets":                     []string{mgr},
			"@Redfish.OperationApplyTime": "Immediate",
			"Oem": map[string]any{"Supermicro": map[string]any{"BMC": map[string]any{
				"PreserveCfg": true, "PreserveSdr": true, "PreserveSsl": true, "BackupBMC": true,
			}}},
		})
	}
	params := map[string]any{}
	// The manager names its running firmware; without the link the choice of
	// target is left to the service, which is what an absent Targets means.
	var doc struct {
		Links struct {
			ActiveSoftwareImage ref `json:"ActiveSoftwareImage"`
		} `json:"Links"`
	}
	if err := c.managerDoc(&doc); err != nil {
		return nil, err
	}
	if target := doc.Links.ActiveSoftwareImage.ODataID; target != "" {
		params["Targets"] = []string{target}
	}
	// Immediate is the default apply time; it is spelled out only where the
	// service says it takes the annotation, so that one offering a choice
	// cannot pick staging.
	if slices.Contains(us.ApplyTime.SupportedValues, "Immediate") {
		params["@Redfish.OperationApplyTime"] = "Immediate"
	}
	return json.Marshal(params)
}

// updateForm frames the two parts of a multipart push.
func updateForm(params, image []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, part := range []struct {
		disposition, contentType string
		data                     []byte
	}{
		{`form-data; name="UpdateParameters"`, "application/json", params},
		{`form-data; name="UpdateFile"; filename="firmware.bin"`, "application/octet-stream", image},
	} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", part.disposition)
		h.Set("Content-Type", part.contentType)
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := w.Write(part.data); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

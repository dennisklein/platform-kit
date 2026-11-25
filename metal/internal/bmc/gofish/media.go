package gofish

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"time"

	"github.com/stmcginnis/gofish/schemas"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/poll"
	"github.com/dennisklein/platform-kit/metal/internal/redact"
)

// vmmControlAction is the iBMC's OEM virtual-media action.
const vmmControlAction = "#VirtualMedia.VmmControl"

// mediaCollection finds the virtual-media collection. The Manager's link is
// asked first: it is the location both captured vendors serve and the one
// proven on them (the X12 serves the System link as well). An endpoint whose
// Manager has no such link keeps the collection under the ComputerSystem —
// where the specification moved it.
//
// The answer is kept for the session.
func (c *client) mediaCollection() (string, error) {
	c.memo.Lock()
	coll := c.memo.mediaColl
	c.memo.Unlock()
	if coll != nil {
		return *coll, nil
	}
	uri, err := c.findMediaCollection()
	if err != nil {
		return "", err
	}
	c.memo.Lock()
	c.memo.mediaColl = &uri
	c.memo.Unlock()
	return uri, nil
}

func (c *client) findMediaCollection() (string, error) {
	var doc struct {
		VirtualMedia ref `json:"VirtualMedia"`
	}
	if err := c.managerDoc(&doc); err != nil {
		return "", err
	}
	if doc.VirtualMedia.ODataID != "" {
		return doc.VirtualMedia.ODataID, nil
	}
	systems, err := c.svc.Systems()
	if err != nil {
		return "", fmt.Errorf("bmc: systems: %w", err)
	}
	slices.SortFunc(systems, func(a, b *schemas.ComputerSystem) int { return compareIDs(a.ID, b.ID) })
	for _, s := range systems {
		doc.VirtualMedia = ref{}
		if _, err := c.getJSON(s.ODataID, &doc); err != nil {
			return "", fmt.Errorf("bmc: system: %w", err)
		}
		if doc.VirtualMedia.ODataID != "" {
			return doc.VirtualMedia.ODataID, nil
		}
	}
	return "", nil
}

// slot is one virtual-media resource with the parts gofish does not type: the
// OEM actions and their targets.
type slot struct {
	*schemas.VirtualMedia
	oemTargets map[string]string
}

// slots reads the collection in the order the service lists it — the slot
// order is the fallback tie-break for callers choosing one, so it must be the
// service's and not the order the reads happened to finish in.
func (c *client) slots() ([]slot, error) {
	coll, err := c.mediaCollection()
	if err != nil {
		return nil, err
	}
	if coll == "" {
		return nil, nil
	}
	uris, err := c.members(coll)
	if err != nil {
		return nil, fmt.Errorf("bmc: virtual media: %w", err)
	}
	out := make([]slot, 0, len(uris))
	for _, uri := range uris {
		vm, err := schemas.GetObject[schemas.VirtualMedia](c.api, uri)
		if err != nil {
			return nil, fmt.Errorf("bmc: virtual media %s: %w", uri, err)
		}
		out = append(out, slot{VirtualMedia: vm, oemTargets: oemActions(vm.RawData)})
	}
	return out, nil
}

// oemActions collects a resource's OEM actions from the three places services
// declare them: Actions.Oem["#Name"] (the specification), Actions.Oem.<Vendor>
// ["#Name"] (a common variant), and Oem.<Vendor>.Actions["#Name"] — where the
// iBMC puts VmmControl, with a target outside the Actions path. The target is
// always taken from the declaration, never composed.
func oemActions(raw []byte) map[string]string {
	type action struct {
		Target string `json:"target"`
	}
	var doc struct {
		Actions struct {
			Oem map[string]json.RawMessage `json:"Oem"`
		} `json:"Actions"`
		Oem map[string]struct {
			Actions map[string]action `json:"Actions"`
		} `json:"Oem"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := map[string]string{}
	add := func(name string, a action) {
		if a.Target != "" && out[name] == "" {
			out[name] = a.Target
		}
	}
	// Keys are walked in sorted order: when two vendor blocks declare the
	// same action, the first one by name wins on every read, not whichever
	// map iteration happens to visit first.
	for _, name := range slices.Sorted(maps.Keys(doc.Actions.Oem)) {
		body := doc.Actions.Oem[name]
		var direct action
		if json.Unmarshal(body, &direct) == nil && direct.Target != "" {
			add(name, direct)
			continue
		}
		var nested map[string]action
		if json.Unmarshal(body, &nested) == nil {
			for _, n := range slices.Sorted(maps.Keys(nested)) {
				add(n, nested[n])
			}
		}
	}
	for _, vendor := range slices.Sorted(maps.Keys(doc.Oem)) {
		acts := doc.Oem[vendor].Actions
		for _, name := range slices.Sorted(maps.Keys(acts)) {
			add(name, acts[name])
		}
	}
	return out
}

func (c *client) Media(ctx context.Context) ([]bmc.VirtualMedia, error) {
	slots, err := c.slots()
	if err != nil {
		return nil, err
	}
	var out []bmc.VirtualMedia
	for _, vm := range slots {
		entry := bmc.VirtualMedia{
			ID:             vm.ID,
			InsertStandard: vm.SupportsMediaInsert,
			Inserted:       vm.Inserted != nil && *vm.Inserted,
			ImageURL:       vm.Image,
		}
		for _, mt := range vm.MediaTypes {
			entry.MediaTypes = append(entry.MediaTypes, string(mt))
		}
		for name := range vm.oemTargets {
			entry.OemActions = append(entry.OemActions, name)
		}
		slices.Sort(entry.OemActions)
		out = append(out, entry)
	}
	return out, nil
}

func (c *client) slotByID(id string) (slot, error) {
	slots, err := c.slots()
	if err != nil {
		return slot{}, err
	}
	for _, vm := range slots {
		if vm.ID == id {
			return vm, nil
		}
	}
	return slot{}, fmt.Errorf("bmc: no media slot %q", id)
}

// InsertMedia masks the error text before it leaves the client: a service
// that refuses a mount echoes the image URL, which carries a grant token and,
// on the vmm dialect, the grant credentials as userinfo. Whoever records the
// error then cannot store them by mistake.
func (c *client) InsertMedia(ctx context.Context, mediaID, imageURL string, creds *bmc.MediaCredentials) error {
	var secret string
	if creds != nil {
		secret = creds.Password
	}
	return redact.Error(c.insertMedia(ctx, mediaID, imageURL, creds), secret)
}

func (c *client) insertMedia(ctx context.Context, mediaID, imageURL string, creds *bmc.MediaCredentials) error {
	if c.readOnly {
		return errReadOnly
	}
	vm, err := c.slotByID(mediaID)
	if err != nil {
		return err
	}
	if vm.SupportsMediaInsert {
		t := true
		// Image, Inserted and WriteProtected only — the payload proven on the
		// X12. The transfer protocol is left unnamed: a service derives it
		// from the URL scheme.
		params := &schemas.VirtualMediaInsertMediaParameters{
			Image: imageURL, Inserted: &t, WriteProtected: &t,
		}
		if creds != nil {
			// Credentials are action parameters here — userinfo in the URL
			// is rejected at the Image schema.
			params.UserName, params.Password = &creds.Username, &creds.Password
		}
		info, err := vm.InsertMedia(params)
		if err != nil {
			return fmt.Errorf("bmc: InsertMedia: %w", err)
		}
		if err := c.followTask(ctx, info); err != nil {
			return err
		}
		return c.awaitInserted(ctx, mediaID)
	}
	if target := vm.oemTargets[vmmControlAction]; target != "" {
		if creds != nil {
			// iBMC's only credential mechanism is userinfo in the image URL.
			if imageURL, err = withUserinfo(imageURL, creds); err != nil {
				return err
			}
		}
		if err := c.vmmControl(ctx, target, "Connect", imageURL); err != nil {
			return err
		}
		return c.awaitInserted(ctx, mediaID)
	}
	return fmt.Errorf("bmc: media %q advertises no insert mechanism", mediaID)
}

// insertTimeout bounds the wait for a slot to read Inserted after an insert
// the service accepted.
const insertTimeout = 30 * time.Second

// awaitInserted reads the slot back until it reports Inserted. An accepted
// insert is not a mount: a service may answer before the mount, or take the
// action and fail the mount later, and the machine would then boot with no
// medium.
func (c *client) awaitInserted(ctx context.Context, mediaID string) error {
	ctx, cancel := context.WithTimeout(ctx, insertTimeout)
	defer cancel()
	err := poll.Until(ctx, taskBackoff, func(context.Context) (bool, error) {
		vm, err := c.slotByID(mediaID)
		if err != nil {
			return false, err
		}
		// A slot that serves no Inserted cannot say; it is taken as mounted.
		return vm.Inserted == nil || *vm.Inserted, nil
	})
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("bmc: media %s does not read inserted %s after the insert: %w", mediaID, insertTimeout, err)
	}
	return err
}

func withUserinfo(imageURL string, creds *bmc.MediaCredentials) (string, error) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return "", fmt.Errorf("bmc: image url: %w", err)
	}
	u.User = url.UserPassword(creds.Username, creds.Password)
	return u.String(), nil
}

// EjectMedia masks its error text as InsertMedia does: a refusal can name
// the URL the slot holds.
func (c *client) EjectMedia(ctx context.Context, mediaID string) error {
	return redact.Error(c.ejectMedia(ctx, mediaID))
}

func (c *client) ejectMedia(ctx context.Context, mediaID string) error {
	if c.readOnly {
		return errReadOnly
	}
	vm, err := c.slotByID(mediaID)
	if err != nil {
		return err
	}
	if vm.SupportsMediaEject {
		info, err := vm.EjectMedia()
		if err != nil {
			return fmt.Errorf("bmc: EjectMedia: %w", err)
		}
		return c.followTask(ctx, info)
	}
	if target := vm.oemTargets[vmmControlAction]; target != "" {
		return c.vmmControl(ctx, target, "Disconnect", "")
	}
	return fmt.Errorf("bmc: media %q advertises no eject mechanism", mediaID)
}

// vmmControl drives the Huawei OEM action: POST, expect 202 and a task
// monitor, poll it to completion.
func (c *client) vmmControl(ctx context.Context, target, controlType, image string) error {
	payload := map[string]string{"VmmControlType": controlType}
	if image != "" {
		payload["Image"] = image
	}
	resp, err := c.api.Post(target, payload)
	if err != nil {
		return fmt.Errorf("bmc: VmmControl %s: %w", controlType, err)
	}
	defer resp.Body.Close()
	return c.pollAccepted(ctx, resp)
}

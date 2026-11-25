package gofish

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/stmcginnis/gofish/schemas"
)

// accountRole is the role every account this client creates gets: the
// operator's own account has to do everything the factory one can.
const accountRole = "Administrator"

// EnsureAccount creates or resets the named manager account over the standard
// AccountService. There are three shapes of it in the field, tried in the
// order that never disturbs another account:
//
//   - the username exists: PATCH that member's password, and clear its lock
//     where the member serves one — a new password does not unlock an
//     account on every BMC, and failed logins with the old one lock it;
//   - the collection is a fixed set of slots, some vacant (an empty UserName):
//     PATCH the first vacant slot that takes it — such a collection takes no
//     POST, and some keep a vacant slot read-only (iDRAC's slot 1), which
//     refuses the PATCH with 400 or 403;
//   - otherwise: POST a new member.
//
// Raw writes, not gofish's typed helpers — the payload must be exactly the
// properties named here, nothing a schema round-trip might add.
func (c *client) EnsureAccount(ctx context.Context, username, password string) error {
	if c.readOnly {
		return errReadOnly
	}
	as, err := c.svc.AccountService()
	if err != nil {
		return fmt.Errorf("bmc: account service: %w", err)
	}
	if as == nil || as.ODataID == "" {
		// gofish answers a nil service without error when the root has no link.
		return fmt.Errorf("bmc: endpoint exposes no account service")
	}
	accounts, err := as.Accounts()
	if err != nil {
		return fmt.Errorf("bmc: accounts: %w", err)
	}
	slices.SortFunc(accounts, func(a, b *schemas.ManagerAccount) int { return compareIDs(a.ID, b.ID) })
	for _, a := range accounts {
		if a.UserName != username {
			continue
		}
		reset := map[string]any{"Password": password, "Enabled": true}
		if servesProperty(a.RawData, "Locked") {
			reset["Locked"] = false
		}
		if err := c.patch(a.ODataID, reset); err != nil {
			return fmt.Errorf("bmc: account %s: %w", username, err)
		}
		return nil
	}
	create := map[string]any{
		"UserName": username, "Password": password, "RoleId": accountRole, "Enabled": true,
	}
	var refused error
	for _, a := range accounts {
		if a.UserName != "" {
			continue
		}
		err := c.patch(a.ODataID, create)
		if err == nil {
			return nil
		}
		if st := statusOf(err); st != http.StatusBadRequest && st != http.StatusForbidden {
			return fmt.Errorf("bmc: account %s in slot %s: %w", username, a.ID, err)
		}
		refused = cmp.Or(refused, fmt.Errorf("bmc: account %s in slot %s: %w", username, a.ID, err))
	}
	if refused != nil {
		return refused
	}
	uri, err := c.accountsCollection(as.ODataID)
	if err != nil {
		return err
	}
	resp, err := c.api.Post(uri, create)
	if err != nil {
		if statusOf(err) == http.StatusMethodNotAllowed {
			return fmt.Errorf("bmc: create account %s: every account slot is taken: %w", username, err)
		}
		return fmt.Errorf("bmc: create account %s: %w", username, err)
	}
	resp.Body.Close()
	return nil
}

// DisableAccount PATCHes Enabled false on the member of that name. Every
// account service in the field takes it, fixed-slot collections included,
// where a DELETE is refused; a disabled account also stays visible to a
// person who looks at the BMC, which a vacated slot would not.
func (c *client) DisableAccount(ctx context.Context, username string) error {
	if c.readOnly {
		return errReadOnly
	}
	if username == "" {
		return fmt.Errorf("bmc: disable account: no username")
	}
	as, err := c.svc.AccountService()
	if err != nil {
		return fmt.Errorf("bmc: account service: %w", err)
	}
	if as == nil || as.ODataID == "" {
		return fmt.Errorf("bmc: endpoint exposes no account service")
	}
	accounts, err := as.Accounts()
	if err != nil {
		return fmt.Errorf("bmc: accounts: %w", err)
	}
	for _, a := range accounts {
		if a.UserName != username {
			continue
		}
		if !a.Enabled {
			return nil
		}
		if err := c.patch(a.ODataID, map[string]any{"Enabled": false}); err != nil {
			return fmt.Errorf("bmc: disable account %s: %w", username, err)
		}
		return nil
	}
	return nil
}

// servesProperty reports whether a resource's JSON carries the property.
func servesProperty(raw []byte, name string) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	_, ok := doc[name]
	return ok
}

// accountsCollection reads the Accounts link off the raw AccountService —
// gofish keeps the collection URI private.
func (c *client) accountsCollection(uri string) (string, error) {
	var doc struct {
		Accounts ref `json:"Accounts"`
	}
	if _, err := c.getJSON(uri, &doc); err != nil {
		return "", fmt.Errorf("bmc: account service: %w", err)
	}
	if doc.Accounts.ODataID == "" {
		return "", fmt.Errorf("bmc: account service carries no Accounts collection")
	}
	return doc.Accounts.ODataID, nil
}

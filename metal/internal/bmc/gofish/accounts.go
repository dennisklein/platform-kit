package gofish

import (
	"context"
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
//   - the username exists: PATCH that member's password;
//   - the collection is a fixed set of slots, some vacant (an empty UserName):
//     PATCH the first vacant slot — such a collection takes no POST;
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
		if err := c.patch(a.ODataID, map[string]any{"Password": password, "Enabled": true}); err != nil {
			return fmt.Errorf("bmc: account %s: %w", username, err)
		}
		return nil
	}
	create := map[string]any{
		"UserName": username, "Password": password, "RoleId": accountRole, "Enabled": true,
	}
	for _, a := range accounts {
		if a.UserName != "" {
			continue
		}
		if err := c.patch(a.ODataID, create); err != nil {
			return fmt.Errorf("bmc: account %s in slot %s: %w", username, a.ID, err)
		}
		return nil
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

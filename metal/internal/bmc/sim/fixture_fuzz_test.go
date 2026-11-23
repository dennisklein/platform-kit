package sim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzFixtureValidate: an accepted fixture tells its systems and NICs apart
// (unique system ids, unique MACs in any case), and the sim serves its tree
// without a panic.
func FuzzFixtureValidate(f *testing.F) {
	for _, name := range Fixtures() {
		data, err := embedded.ReadFile("fixtures/" + name + ".yaml")
		require.NoError(f, err)
		f.Add(data)
	}
	f.Add([]byte("systems: [{ id: \"1\" }]\n"))
	f.Add([]byte("systems: []\n"))
	f.Add([]byte("layout: { drives: nowhere }\nsystems: [{ id: \"1\" }]\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		fx, err := parseFixture(raw)
		if err != nil {
			return
		}
		ids, macs := map[string]bool{}, map[string]bool{}
		mac := func(m string) {
			if m == "" {
				return
			}
			key := strings.ToLower(m)
			assert.False(t, macs[key], "MAC %q twice", m)
			macs[key] = true
		}
		mac(fx.Manager.MAC)
		for _, s := range fx.Systems {
			assert.False(t, ids[s.ID], "system id %q twice", s.ID)
			ids[s.ID] = true
			for _, n := range s.NICs {
				mac(n.MAC)
			}
		}
		var user, pass string
		if len(fx.Accounts) > 0 {
			user, pass = fx.Accounts[0].Username, fx.Accounts[0].Password
		}
		assert.Greater(t, serveTree(NewServer(NewState(fx)), user, pass), 0)
	})
}

// serveTree reads every resource reachable from the service root, at most
// a few hundred, as the given account. A panic in a handler fails the fuzz
// target. It returns how many resources answered 200.
func serveTree(h http.Handler, user, pass string) int {
	ok := 0
	seen := map[string]bool{"/redfish/v1": true}
	queue := []string{"/redfish/v1"}
	for len(queue) > 0 && len(seen) < 300 {
		uri := queue[0]
		queue = queue[1:]
		rec := httptest.NewRecorder()
		// A fixture id can make a link no client could request; skip it.
		req, err := http.NewRequest(http.MethodGet, uri, nil)
		if err != nil {
			continue
		}
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			ok++
		}
		var doc any
		if json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
			continue
		}
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if id, ok := x["@odata.id"].(string); ok && strings.HasPrefix(id, "/redfish/v1") && !seen[id] {
					seen[id] = true
					queue = append(queue, id)
				}
				for _, c := range x {
					walk(c)
				}
			case []any:
				for _, c := range x {
					walk(c)
				}
			}
		}
		walk(doc)
	}
	return ok
}

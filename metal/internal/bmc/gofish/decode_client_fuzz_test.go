package gofish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	gofishclient "github.com/dennisklein/platform-kit/metal/internal/bmc/gofish"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// crawl lists every resource reachable from the service root, in a stable
// order.
func crawl(t testing.TB, h http.Handler) []string {
	seen := map[string]bool{"/redfish/v1": true}
	queue := []string{"/redfish/v1"}
	for len(queue) > 0 {
		uri := queue[0]
		queue = queue[1:]
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, uri, nil))
		var doc any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
			continue
		}
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if id, ok := x["@odata.id"].(string); ok && !seen[id] {
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
	out := make([]string, 0, len(seen))
	for uri := range seen {
		out = append(out, uri)
	}
	slices.Sort(out)
	require.NotEmpty(t, out)
	return out
}

// override serves the sim tree with one resource replaced.
type override struct {
	next   http.Handler
	mu     sync.Mutex
	uri    string
	status int
	body   []byte
}

func (o *override) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	uri, status, body := o.uri, o.status, o.body
	o.mu.Unlock()
	if r.URL.Path != uri || r.Method != http.MethodGet {
		o.next.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// FuzzClientDecode serves the sim tree with one resource, picked by the
// fuzzer, replaced by arbitrary bytes, and runs the inventory reads. BMC
// firmware is untrusted: the client must not panic, must return in bounded
// time, and must not present a value the body did not hold as a fact.
func FuzzClientDecode(f *testing.F) {
	fx, err := sim.LoadFixture("huawei-2288h-v5")
	require.NoError(f, err)
	tree := sim.NewServer(sim.NewState(fx))
	uris := crawl(f, tree)
	systemIdx := slices.Index(uris, "/redfish/v1/Systems/1")
	require.GreaterOrEqual(f, systemIdx, 0)

	f.Add(uint16(systemIdx), []byte(`{"@odata.id":"/redfish/v1/Systems/1","Id":"1"}`), uint16(200))
	f.Add(uint16(systemIdx), []byte(`{"Id":"1","PowerState":"On"}`), uint16(200))
	f.Add(uint16(systemIdx), []byte(`{"Id":"1","PowerState":7}`), uint16(200))
	f.Add(uint16(systemIdx), []byte(`{`), uint16(200))
	f.Add(uint16(systemIdx), []byte(``), uint16(500))
	for i := range uris {
		f.Add(uint16(i), []byte(`{"Members":[{"@odata.id":"/redfish/v1/Nowhere"}]}`), uint16(200))
		// A JSON null as a collection made gofish dereference a nil
		// collection in a goroutine of its own and crash the process.
		f.Add(uint16(i), []byte(`null`), uint16(200))
		f.Add(uint16(i), []byte(`[]`), uint16(200))
	}

	o := &override{next: tree}
	srv := httptest.NewServer(o)
	f.Cleanup(srv.Close)

	f.Fuzz(func(t *testing.T, pick uint16, body []byte, status uint16) {
		code := int(status)
		if code < 200 || code > 599 {
			code = 200 + code%400
		}
		uri := uris[int(pick)%len(uris)]
		o.mu.Lock()
		o.uri, o.status, o.body = uri, code, body
		o.mu.Unlock()

		done := make(chan []bmc.System, 1)
		go func() {
			ctx := context.Background()
			c, err := gofishclient.Connect(ctx, gofishclient.Config{Endpoint: srv.URL})
			if err != nil {
				done <- nil
				return
			}
			defer c.Close()
			systems, _ := c.Systems(ctx)
			_, _ = c.Media(ctx)
			_, _ = c.Manager(ctx)
			_, _ = c.Chassis(ctx)
			_, _ = c.BiosSchema(ctx, "1")
			_, _ = c.FirmwareInventory(ctx)
			_, _ = c.Capabilities(ctx)
			done <- systems
		}()
		var systems []bmc.System
		select {
		case systems = <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("inventory reads did not return with %s replaced", uri)
		}

		if uri != "/redfish/v1/Systems/1" || code != http.StatusOK {
			return
		}
		for _, s := range systems {
			if s.PowerState != "" {
				assert.True(t, bytes.Contains(body, []byte(s.PowerState)),
					"PowerState %q is not in the body %q", s.PowerState, body)
			}
		}
	})
}

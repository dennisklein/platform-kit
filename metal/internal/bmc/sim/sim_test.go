package sim_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/asset"
	"github.com/dennisklein/platform-kit/metal/internal/bmc"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/bmctest"
	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
	"github.com/dennisklein/platform-kit/metal/internal/testpki"
)

func TestConformance(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			bmctest.Run(t, sim.NewState(f), f, false)
		})
	}
}

// TestTree checks every personality's HTTP tree at the protocol level: links
// that resolve, absent resources that answer 404 in the error format, the
// boot override as served state, and the declared protocol features.
func TestTree(t *testing.T) {
	for _, name := range sim.Fixtures() {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
			defer srv.Close()
			bmctest.RunTree(t, srv.URL, f)
		})
	}
}

// TestConformanceWithAccounts runs the same suite over enforcement-enabled
// fixtures — the account lane active, everything else identical.
func TestConformanceWithAccounts(t *testing.T) {
	for _, name := range []string{"huawei-2288h-v5", "supermicro-x12"} {
		t.Run(name, func(t *testing.T) {
			f, err := sim.LoadFixture(name)
			require.NoError(t, err)
			f.Accounts = []sim.FixtureAccount{
				{ID: "2", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
			}
			bmctest.Run(t, sim.NewState(f), f, false)
		})
	}
}

func TestAccounts(t *testing.T) {
	ctx := context.Background()

	t.Run("open-sim", func(t *testing.T) {
		f, err := sim.LoadFixture("supermicro-x12")
		require.NoError(t, err)
		st := sim.NewState(f)
		assert.True(t, st.Authenticate("anyone", "anything"), "an open sim enforces nothing")
		assert.Error(t, st.EnsureAccount(ctx, "pk-metal", "Pass12345"),
			"an open sim cannot grow an account service mid-flight")
	})

	t.Run("enforcement", func(t *testing.T) {
		f, err := sim.LoadFixture("supermicro-x12")
		require.NoError(t, err)
		f.Accounts = []sim.FixtureAccount{
			// The captured X12 shape: the well-known ADMIN slot exists but is disabled.
			{ID: "2", Username: "ADMIN", Password: "ADMIN", Role: "Administrator", Enabled: false},
			{ID: "3", Username: "admin", Password: "factory", Role: "Administrator", Enabled: true},
		}
		st := sim.NewState(f)
		assert.True(t, st.Authenticate("admin", "factory"))
		assert.False(t, st.Authenticate("admin", "wrong"))
		assert.False(t, st.Authenticate("ADMIN", "ADMIN"), "a disabled account never authenticates")

		require.NoError(t, st.EnsureAccount(ctx, "pk-metal", "InitialPass0"))
		assert.True(t, st.Authenticate("pk-metal", "InitialPass0"))
		pw, ok := st.AccountPassword("pk-metal")
		require.True(t, ok)
		assert.Equal(t, "InitialPass0", pw)

		require.NoError(t, st.EnsureAccount(ctx, "pk-metal", "RotatedPass1"))
		assert.False(t, st.Authenticate("pk-metal", "InitialPass0"), "the old credential dies with the rotation")
		assert.True(t, st.Authenticate("pk-metal", "RotatedPass1"))

		require.NoError(t, st.SetAccountPassword("pk-metal", "scrambled"), "the out-of-band change")
		assert.False(t, st.Authenticate("pk-metal", "RotatedPass1"))
	})
}

// TestIdentityApply pins the certificate lifecycle: an uploaded identity is
// staged, a manager reset makes it the served one, and the cert-stuck fault
// keeps serving the old identity while every call answered success.
func TestIdentityApply(t *testing.T) {
	ctx := context.Background()
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)

	seed, err := asset.EphemeralCert()
	require.NoError(t, err)
	st.SetServedCertificate(seed)
	before := st.ServedLeaf()
	require.NotNil(t, before)

	certPEM, keyPEM := selfSigned(t, "bmc-new.metal.internal")
	require.NoError(t, st.UploadServerIdentity(ctx, certPEM, keyPEM))
	assert.Equal(t, before.SerialNumber, st.ServedLeaf().SerialNumber,
		"an upload stages — serving switches only at the manager reset")

	require.NoError(t, st.ResetManager(ctx, bmc.ActionGracefulRestart))
	assert.Equal(t, "bmc-new.metal.internal", st.ServedLeaf().Subject.CommonName,
		"the manager reset applies the staged identity")

	st.SetFault(sim.FaultCertStuck, true)
	certPEM, keyPEM = selfSigned(t, "bmc-stuck.metal.internal")
	require.NoError(t, st.UploadServerIdentity(ctx, certPEM, keyPEM),
		"the lying oracle answers success")
	require.NoError(t, st.ResetManager(ctx, bmc.ActionGracefulRestart))
	assert.Equal(t, "bmc-new.metal.internal", st.ServedLeaf().Subject.CommonName,
		"the stuck fault keeps serving the old identity")
}

func selfSigned(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	return testpki.SelfSigned(t, cn, []string{cn}, nil)
}

func TestFaultInjection(t *testing.T) {
	f, err := sim.LoadFixture("supermicro-x12")
	require.NoError(t, err)
	st := sim.NewState(f)
	ctx := context.Background()

	st.SetFault(sim.FaultUnreachable, true)
	_, err = st.Systems(ctx)
	assert.Error(t, err, "unreachable fault must fail reads")
	st.SetFault(sim.FaultUnreachable, false)

	// The lying oracle: power off, but the BMC keeps reporting On.
	st.SetFault(sim.FaultStalePower, true)
	require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceOff))
	state, err := st.Power(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOn, state, "stale-power fault must report the lie")

	st.SetFault(sim.FaultStalePower, false)
	state, err = st.Power(ctx, "1")
	require.NoError(t, err)
	assert.Equal(t, bmc.PowerOff, state, "without the fault the truth returns")
}

// TestHTTPTree asserts the curl-visible shape: the tree walks, and each vendor exposes
// exactly its own dialect (the standard action 404s on Huawei; VmmControl answers 202 +
// a task URI that reports Completed).
func TestHTTPTree(t *testing.T) {
	get := func(t *testing.T, srv *httptest.Server, path string) map[string]any {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
		var v map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
		return v
	}
	post := func(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
		t.Helper()
		b, err := json.Marshal(body)
		require.NoError(t, err)
		resp, err := srv.Client().Post(srv.URL+path, "application/json", bytes.NewReader(b))
		require.NoError(t, err)
		resp.Body.Close()
		return resp
	}

	// The sim fetches at mount time, so inserted URLs must really serve an image.
	asset := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "talos.iso", time.Time{}, bytes.NewReader(make([]byte, 2048)))
	}))
	defer asset.Close()
	img := asset.URL + "/talos.iso"

	t.Run("supermicro-standard-insert", func(t *testing.T) {
		f, err := sim.LoadFixture("supermicro-x12")
		require.NoError(t, err)
		srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
		defer srv.Close()

		root := get(t, srv, "/redfish/v1")
		require.NotNil(t, root["Systems"], "ServiceRoot lacks Systems link")

		sys := get(t, srv, "/redfish/v1/Systems/1")
		assert.Equal(t, "", sys["SerialNumber"], "X12 Systems/1 serial (the blind spot)")

		chas := get(t, srv, "/redfish/v1/Chassis/1")
		assert.Equal(t, "", chas["SerialNumber"], "X12 chassis SerialNumber lives in Oem")
		oem, _ := chas["Oem"].(map[string]any)
		smc, _ := oem["Supermicro"].(map[string]any)
		assert.Equal(t, "SIMX12B000001", smc["BoardSerialNumber"])

		resp := post(t, srv, "/redfish/v1/Managers/1/VirtualMedia/VirtualMedia1/Actions/VirtualMedia.InsertMedia",
			map[string]string{"Image": img})
		assert.Equal(t, http.StatusNoContent, resp.StatusCode, "standard InsertMedia")

		vm := get(t, srv, "/redfish/v1/Managers/1/VirtualMedia/VirtualMedia1")
		assert.Equal(t, true, vm["Inserted"], "Inserted after InsertMedia")
	})

	t.Run("huawei-oem-only", func(t *testing.T) {
		f, err := sim.LoadFixture("huawei-2288h-v5")
		require.NoError(t, err)
		srv := httptest.NewServer(sim.NewServer(sim.NewState(f)))
		defer srv.Close()

		// The defining Huawei fact: the DMTF action is NOT there.
		resp := post(t, srv, "/redfish/v1/Managers/1/VirtualMedia/CD/Actions/VirtualMedia.InsertMedia",
			map[string]string{"Image": img})
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "standard InsertMedia on iBMC")

		resp = post(t, srv, "/redfish/v1/Managers/1/VirtualMedia/CD/Oem/Huawei/Actions/VirtualMedia.VmmControl",
			map[string]string{"VmmControlType": "Connect", "Image": img})
		require.Equal(t, http.StatusAccepted, resp.StatusCode, "VmmControl Connect")

		task := get(t, srv, resp.Header.Get("Location"))
		assert.Equal(t, "Completed", task["TaskState"])

		vm := get(t, srv, "/redfish/v1/Managers/1/VirtualMedia/CD")
		assert.Equal(t, true, vm["Inserted"], "Inserted after VmmControl Connect")
	})
}

// TestMediaFetch pins the sim's media-client behaviour: the dialect-shaped
// mount-time reads, the full pull at power-on, and the URL rules both vendors
// enforce.
func TestMediaFetch(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte{0xAB}, 3000)
	var mu sync.Mutex
	var reqs []string
	asset := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "node.iso", time.Time{}, bytes.NewReader(payload))
	}))
	defer asset.Close()
	img := asset.URL + "/node.iso"

	t.Run("standard-mount-sniff-and-boot-pull", func(t *testing.T) {
		f, err := sim.LoadFixture("supermicro-x12")
		require.NoError(t, err)
		st := sim.NewState(f)
		mu.Lock()
		reqs = nil
		mu.Unlock()

		require.NoError(t, st.InsertMedia(ctx, "VirtualMedia1", img, nil))
		mu.Lock()
		mounted := append([]string(nil), reqs...)
		mu.Unlock()
		require.Equal(t, []string{"HEAD ", "GET bytes=0-511"}, mounted,
			"the X12 HEADs then sniffs the first sector at mount time")
		assert.Nil(t, st.BootImage("VirtualMedia1"), "no full pull before boot")

		require.NoError(t, st.SetPower(ctx, "1", bmc.ActionForceRestart))
		assert.Equal(t, payload, st.BootImage("VirtualMedia1"), "the boot pulls the full image")
	})

	t.Run("vmm-userinfo-credentials", func(t *testing.T) {
		guard := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user, pass, ok := r.BasicAuth(); !ok || user != "media" || pass != "grant" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			http.ServeContent(w, r, "node.iso", time.Time{}, bytes.NewReader(payload))
		}))
		defer guard.Close()
		f, err := sim.LoadFixture("huawei-2288h-v5")
		require.NoError(t, err)
		st := sim.NewState(f)

		u, err := url.Parse(guard.URL)
		require.NoError(t, err)
		withCreds := "https://media:grant@" + u.Host + "/node.iso"
		require.NoError(t, st.InsertMedia(ctx, "CD", withCreds, nil),
			"iBMC credentials ride as URL userinfo")
		require.NoError(t, st.EjectMedia(ctx, "CD"))

		wrong := "https://media:not-the-grant@" + u.Host + "/node.iso"
		assert.ErrorContains(t, st.InsertMedia(ctx, "CD", wrong, nil),
			"failed to connect to virtual media", "a refused fetch fails the mount")
	})

	t.Run("url-rules", func(t *testing.T) {
		f, err := sim.LoadFixture("supermicro-x12")
		require.NoError(t, err)
		st := sim.NewState(f)

		assert.ErrorContains(t, st.InsertMedia(ctx, "VirtualMedia1", "http://x/node.iso", nil), "https")
		assert.ErrorContains(t, st.InsertMedia(ctx, "VirtualMedia1", img+"?v=1", nil), "incorrect file format")
		assert.ErrorContains(t, st.InsertMedia(ctx, "VirtualMedia1", asset.URL+"/node.img", nil), "incorrect file format")
		creds := "https://media:grant@" + img[len("https://"):]
		assert.ErrorContains(t, st.InsertMedia(ctx, "VirtualMedia1", creds, nil), "userinfo",
			"the X12 rejects userinfo at the Image schema")
	})
}

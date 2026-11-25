package bmctest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dennisklein/platform-kit/metal/internal/bmc/sim"
)

// RunTree checks the Redfish tree behind baseURL at the protocol level — what
// no bmc.Client can see, because a client only asks for what it knows about:
// that every link leads to a resource which names itself, that a URI nothing
// lives at answers 404 in the protocol's error format (a client probing for an
// optional service relies on that "no"), that the boot override is state the
// service keeps and serves, and — where the fixture declares them — ETags,
// preconditions and sessions. It runs against every personality.
func RunTree(t *testing.T, baseURL string, f *sim.Fixture) {
	t.Helper()
	tr := &tree{t: t, base: strings.TrimRight(baseURL, "/")}
	if len(f.Accounts) > 0 {
		tr.user, tr.pass = f.Accounts[0].Username, f.Accounts[0].Password
	}

	t.Run("service-root", func(t *testing.T) {
		tr.t = t
		resp, body := tr.do(http.MethodGet, "/redfish", nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "/redfish/v1/", body["v1"], "the version document names the v1 root")

		// The root is public: a client has to read it before it can know how
		// to authenticate.
		resp, root := tr.anonymous(http.MethodGet, "/redfish/v1", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "the service root needs no credentials")
		assert.Equal(t, "4.0", resp.Header.Get("OData-Version"))
		assert.Regexp(t, `^\d+\.\d+\.\d+$`, root["RedfishVersion"])
		for _, service := range []string{"Systems", "Managers", "Chassis", "UpdateService", "Tasks", "EventService", "Registries"} {
			assert.NotEmpty(t, odataID(root[service]), "service root links %s", service)
		}
		if len(f.Accounts) > 0 {
			assert.NotEmpty(t, odataID(root["AccountService"]), "service root links AccountService")
			resp, body := tr.anonymous(http.MethodGet, "/redfish/v1/Systems", nil)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "everything below the root needs credentials")
			assertError(t, body, "")
		}
		if f.Protocol.Sessions {
			assert.NotEmpty(t, odataID(root["SessionService"]), "service root links SessionService")
		}
		if f.Identity.CertificateService {
			assert.NotEmpty(t, odataID(root["CertificateService"]), "service root links CertificateService")
		}
	})

	t.Run("every-link-resolves", func(t *testing.T) {
		tr.t = t
		seen := map[string]bool{}
		queue := []string{"/redfish/v1"}
		for len(queue) > 0 {
			uri := queue[0]
			queue = queue[1:]
			if seen[uri] {
				continue
			}
			seen[uri] = true
			resp, doc := tr.do(http.MethodGet, uri, nil, nil)
			if !assert.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", uri) {
				continue
			}
			assert.Equal(t, uri, doc["@odata.id"], "%s names itself", uri)
			assert.Regexp(t, odataType, doc["@odata.type"], "%s carries its type", uri)
			assert.NotEmpty(t, doc["Name"], "%s carries a Name", uri)
			if members, ok := doc["Members"].([]any); ok {
				assert.EqualValues(t, len(members), doc["Members@odata.count"], "%s member count", uri)
			} else {
				assert.NotEmpty(t, doc["Id"], "%s carries an Id", uri)
			}
			if f.Protocol.ETags {
				assert.NotEmpty(t, resp.Header.Get("ETag"), "%s sends an ETag", uri)
				assert.Equal(t, resp.Header.Get("ETag"), doc["@odata.etag"], "%s header and property agree", uri)
			}
			for _, next := range linksIn(doc) {
				// A fragment points into a document already fetched.
				if !strings.Contains(next, "#") && strings.HasPrefix(next, "/redfish/v1") {
					queue = append(queue, strings.TrimRight(next, "/"))
				}
			}
			// A registry file is found through Location, not through a link,
			// and is a plain JSON document rather than a resource.
			if locations, ok := doc["Location"].([]any); ok {
				for _, l := range locations {
					if loc, _ := l.(map[string]any); loc != nil {
						if file, _ := loc["Uri"].(string); file != "" {
							resp, _ := tr.do(http.MethodGet, file, nil, nil)
							assert.Equal(t, http.StatusOK, resp.StatusCode, "registry file %s", file)
						}
					}
				}
			}
		}
		assert.Greater(t, len(seen), 15, "the walk reached the tree")
	})

	t.Run("absent-resources", func(t *testing.T) {
		tr.t = t
		// Services no personality offers, at the root and below a resource.
		for _, uri := range []string{
			"/redfish/v1/LicenseService",
			"/redfish/v1/Managers/1/LicenseManager",
			"/redfish/v1/Systems/" + f.Systems[0].ID + "/NoSuchSubordinate",
			"/redfish/v2",
			"/not-redfish-at-all",
		} {
			resp, body := tr.do(http.MethodGet, uri, nil, nil)
			if assert.Equal(t, http.StatusNotFound, resp.StatusCode, "GET %s", uri) {
				assertError(t, body, "ResourceMissingAtURI")
			}
		}
		// A member a collection does not have.
		resp, body := tr.do(http.MethodGet, "/redfish/v1/Systems/no-such-system", nil, nil)
		if assert.Equal(t, http.StatusNotFound, resp.StatusCode) {
			assertError(t, body, "")
		}
		// A resource that exists, asked with a method it does not take.
		resp, body = tr.do(http.MethodDelete, "/redfish/v1/Systems", nil, nil)
		if assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode) {
			assert.Contains(t, resp.Header.Get("Allow"), http.MethodGet)
			assertError(t, body, "")
		}
	})

	t.Run("boot-override-is-served", func(t *testing.T) {
		tr.t = t
		fs := f.Systems[0]
		if len(fs.BootTargets) < 2 {
			t.Skip("fixture offers no boot target to set")
		}
		uri := "/redfish/v1/Systems/" + fs.ID
		want := map[string]any{
			"BootSourceOverrideTarget":  fs.BootTargets[1],
			"BootSourceOverrideEnabled": "Continuous",
		}
		if fs.BootMode != "" {
			want["BootSourceOverrideMode"] = "UEFI"
		}
		resp, body := tr.patch(uri, map[string]any{"Boot": want})
		require.Less(t, resp.StatusCode, 300, "PATCH Boot: %v", body)

		_, doc := tr.do(http.MethodGet, uri, nil, nil)
		boot, _ := doc["Boot"].(map[string]any)
		require.NotNil(t, boot, "the system serves a Boot object")
		for property, value := range want {
			assert.Equal(t, value, boot[property], "Boot.%s persists", property)
		}
		assert.NotEmpty(t, boot["BootSourceOverrideTarget@Redfish.AllowableValues"])

		// A partial PATCH changes what it names and nothing else.
		resp, body = tr.patch(uri, map[string]any{"Boot": map[string]any{"BootSourceOverrideEnabled": "Once"}})
		require.Less(t, resp.StatusCode, 300, "PATCH Boot: %v", body)
		_, doc = tr.do(http.MethodGet, uri, nil, nil)
		boot, _ = doc["Boot"].(map[string]any)
		assert.Equal(t, "Once", boot["BootSourceOverrideEnabled"])
		assert.Equal(t, fs.BootTargets[1], boot["BootSourceOverrideTarget"], "the target survives a partial PATCH")

		resp, body = tr.patch(uri, map[string]any{"Boot": map[string]any{"BootSourceOverrideTarget": "NotARealTarget"}})
		if assert.Equal(t, http.StatusBadRequest, resp.StatusCode) {
			assertError(t, body, "PropertyValueNotInList")
		}
		resp, _ = tr.patch(uri, map[string]any{"Boot": map[string]any{"BootSourceOverrideEnabled": "Disabled"}})
		require.Less(t, resp.StatusCode, 300)
	})

	t.Run("etags-and-preconditions", func(t *testing.T) {
		tr.t = t
		if !f.Protocol.ETags {
			// Without ETags a PATCH carrying an If-Match the service never
			// issued must still go through: there is nothing to compare.
			resp, _ := tr.do(http.MethodGet, "/redfish/v1/Systems/"+f.Systems[0].ID, nil, nil)
			assert.Empty(t, resp.Header.Get("ETag"), "the fixture declares no ETags")
			return
		}
		uri := "/redfish/v1/Systems/" + f.Systems[0].ID
		resp, _ := tr.do(http.MethodGet, uri, nil, nil)
		tag := resp.Header.Get("ETag")
		require.NotEmpty(t, tag)

		resp, _ = tr.do(http.MethodGet, uri, nil, map[string]string{"If-None-Match": tag})
		assert.Equal(t, http.StatusNotModified, resp.StatusCode, "an unchanged resource is not sent again")

		change := map[string]any{"Boot": map[string]any{"BootSourceOverrideEnabled": "Disabled"}}
		if f.Protocol.RequireIfMatch {
			resp, body := tr.do(http.MethodPatch, uri, change, nil)
			if assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode, "PATCH without If-Match") {
				assertError(t, body, "PreconditionRequired")
			}
		}
		resp, body := tr.do(http.MethodPatch, uri, change, map[string]string{"If-Match": `W/"0000000000000000"`})
		if assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, "PATCH with a stale tag") {
			assertError(t, body, "PreconditionFailed")
		}
		resp, _ = tr.do(http.MethodPatch, uri, change, map[string]string{"If-Match": tag})
		assert.Less(t, resp.StatusCode, 300, "PATCH with the current tag")
	})

	t.Run("sessions", func(t *testing.T) {
		tr.t = t
		const sessions = "/redfish/v1/SessionService/Sessions"
		if !f.Protocol.Sessions {
			resp, body := tr.do(http.MethodGet, "/redfish/v1/SessionService", nil, nil)
			if assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the fixture declares no session service") {
				assertError(t, body, "ResourceMissingAtURI")
			}
			return
		}
		// A fixture that seeds no account enforces no credential: any login
		// opens a session, and every request passes with or without one.
		enforced := len(f.Accounts) > 0
		if enforced {
			resp, _ := tr.anonymous(http.MethodPost, sessions, map[string]string{"UserName": tr.user, "Password": "not-the-password"})
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a wrong password opens no session")
		}

		resp, body := tr.anonymous(http.MethodPost, sessions, map[string]string{"UserName": tr.user, "Password": tr.pass})
		require.Equal(t, http.StatusCreated, resp.StatusCode, "login: %v", body)
		token, location := resp.Header.Get("X-Auth-Token"), resp.Header.Get("Location")
		require.NotEmpty(t, token)
		require.NotEmpty(t, location)
		assert.Equal(t, location, body["@odata.id"], "the session resource is the Location")

		byToken := map[string]string{"X-Auth-Token": token}
		resp, _ = tr.raw(http.MethodGet, "/redfish/v1/Systems", nil, byToken, false)
		assert.Equal(t, http.StatusOK, resp.StatusCode, "the token authenticates")
		resp, _ = tr.raw(http.MethodDelete, location, nil, byToken, false)
		assert.Less(t, resp.StatusCode, 300, "logout")
		if enforced {
			resp, _ = tr.raw(http.MethodGet, "/redfish/v1/Systems", nil, byToken, false)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a deleted session's token is dead")
		}
	})
}

// odataType is the form of an @odata.type: #Namespace.vMajor_Minor_Errata.Type,
// unversioned for collections.
var odataType = regexp.MustCompile(`^#\w+(\.v\d+_\d+_\d+)?\.\w+$`)

type tree struct {
	t          *testing.T
	base       string
	user, pass string
}

// do sends an authenticated request and decodes a JSON answer.
func (tr *tree) do(method, uri string, body any, headers map[string]string) (*http.Response, map[string]any) {
	return tr.raw(method, uri, body, headers, true)
}

// anonymous sends a request without credentials.
func (tr *tree) anonymous(method, uri string, body any) (*http.Response, map[string]any) {
	return tr.raw(method, uri, body, nil, false)
}

// patch updates a resource the way a careful client does: with the ETag of a
// fresh read as If-Match, when the resource has one.
func (tr *tree) patch(uri string, body any) (*http.Response, map[string]any) {
	resp, _ := tr.do(http.MethodGet, uri, nil, nil)
	var headers map[string]string
	if tag := resp.Header.Get("ETag"); tag != "" {
		headers = map[string]string{"If-Match": tag}
	}
	return tr.do(http.MethodPatch, uri, body, headers)
}

func (tr *tree) raw(method, uri string, body any, headers map[string]string, auth bool) (*http.Response, map[string]any) {
	tr.t.Helper()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(tr.t, err)
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, tr.base+uri, payload)
	require.NoError(tr.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if auth && tr.user != "" {
		req.SetBasicAuth(tr.user, tr.pass)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(tr.t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(tr.t, err)
	var doc map[string]any
	if len(data) > 0 && strings.Contains(resp.Header.Get("Content-Type"), "json") {
		require.NoError(tr.t, json.Unmarshal(data, &doc), "%s %s answers JSON", method, uri)
	}
	return resp, doc
}

// assertError checks an error answer against the specification's format: an
// error object with a code and a message, and extended information naming a
// Base registry message. messageID, when given, is the message it must be.
func assertError(t *testing.T, body map[string]any, messageID string) {
	t.Helper()
	e, _ := body["error"].(map[string]any)
	if !assert.NotNil(t, e, "an error answer carries an error object") {
		return
	}
	assert.Regexp(t, `^Base\.\d+\.\d+\.\w+$`, e["code"], "error.code is a Base registry message id")
	assert.NotEmpty(t, e["message"], "error.message")
	info, _ := e["@Message.ExtendedInfo"].([]any)
	if !assert.NotEmpty(t, info, "error carries @Message.ExtendedInfo") {
		return
	}
	first, _ := info[0].(map[string]any)
	assert.Regexp(t, `^Base\.\d+\.\d+\.\w+$`, first["MessageId"])
	assert.NotEmpty(t, first["Message"])
	if messageID != "" {
		id, _ := first["MessageId"].(string)
		assert.True(t, strings.HasSuffix(id, "."+messageID), "MessageId %q is %s", id, messageID)
	}
}

// odataID reads the target of a link property.
func odataID(v any) string {
	m, _ := v.(map[string]any)
	id, _ := m["@odata.id"].(string)
	return id
}

// linksIn collects every @odata.id below the top level of a document — the
// links it holds, as opposed to its own identity.
func linksIn(doc map[string]any) []string {
	var out []string
	var walk func(v any, top bool)
	walk = func(v any, top bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "@odata.id" {
					if id, _ := child.(string); id != "" && !top {
						out = append(out, id)
					}
					continue
				}
				walk(child, false)
			}
		case []any:
			for _, child := range x {
				walk(child, false)
			}
		}
	}
	walk(doc, true)
	return out
}

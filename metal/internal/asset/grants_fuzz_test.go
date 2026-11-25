package asset

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelGrant is the test's own record of one issued grant.
type modelGrant struct {
	token, asset, filename string
	expires                time.Time
	revoked                bool
}

func (m *modelGrant) live(now time.Time) bool { return !m.revoked && now.Before(m.expires) }

// FuzzGrantTable drives the grant table with a random sequence of Issue,
// Revoke, RevokeAsset, Live, lookup and clock steps, and checks every answer
// against a plain model:
//   - lookup returns a grant only while it is unrevoked and before expiry;
//   - Live never returns a revoked or expired grant, and finds one whenever
//     the model has one for that asset and filename;
//   - RevokeAsset takes down exactly that asset's grants.
func FuzzGrantTable(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0, 2, 3, 4, 5})
	f.Add([]byte{0, 1, 0, 0, 1, 0, 2, 0, 5, 3, 0})
	f.Add([]byte{0, 0, 0, 0, 0, 1, 4, 0, 4, 6, 4, 0})
	// Two live grants for one asset and filename, minted at one instant
	// with one ttl: Live answered with either, in map order.
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 4, 0, 0, 4, 0, 0})
	f.Fuzz(func(t *testing.T, ops []byte) {
		assets := []string{"asset-a", "asset-b", "asset-c"}
		files := []string{"one.iso", "two.iso"}
		now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		table := NewGrants()
		table.now = func() time.Time { return now }
		var model []*modelGrant

		next := func() int {
			if len(ops) == 0 {
				return 0
			}
			b := ops[0]
			ops = ops[1:]
			return int(b)
		}
		for steps := 0; len(ops) > 0 && steps < 200; steps++ {
			switch next() % 7 {
			case 0: // Issue
				a, fn, ttl := assets[next()%3], files[next()%2], time.Duration(next()%4+1)*time.Second
				g, err := table.Issue(a, fn, []byte("x"), ttl, nil)
				require.NoError(t, err)
				model = append(model, &modelGrant{token: g.Token, asset: a, filename: fn, expires: now.Add(ttl)})
			case 1: // Revoke
				if len(model) > 0 {
					m := model[next()%len(model)]
					table.Revoke(m.token)
					m.revoked = true
				}
			case 2: // RevokeAsset
				a := assets[next()%3]
				table.RevokeAsset(a)
				for _, m := range model {
					if m.asset == a {
						m.revoked = true
					}
				}
			case 3: // clock
				now = now.Add(time.Duration(next()%3) * time.Second)
			case 4: // Live
				a, fn := assets[next()%3], files[next()%2]
				g, ok := table.Live(a, fn)
				var want []*modelGrant
				for _, m := range model {
					if m.asset == a && m.filename == fn && m.live(now) {
						want = append(want, m)
					}
				}
				require.Equal(t, len(want) > 0, ok, "Live(%s, %s)", a, fn)
				if ok {
					newest := want[0]
					for _, m := range want[1:] {
						if m.expires.After(newest.expires) ||
							(m.expires.Equal(newest.expires) && m.token > newest.token) {
							newest = m
						}
					}
					assert.Equal(t, newest.token, g.Token, "Live must answer with the newest live grant")
				}
			case 5: // lookup of every token
				for _, m := range model {
					g := table.lookup(m.token)
					assert.Equal(t, m.live(now), g != nil, "lookup(%s) at %s, expires %s, revoked %t",
						m.token, now, m.expires, m.revoked)
				}
			case 6: // lookup of a token that was never issued
				assert.Nil(t, table.lookup(fmt.Sprintf("%032x", next())))
			}
		}
	})
}

// FuzzServePath sends arbitrary requests to the media server. Only a GET or
// HEAD of /grants/<live token>/<its filename>, with the grant's credentials
// where it has some, may succeed. Every refusal of a well-formed grant path
// is the same 401 with the same challenge and body, whether the grant is
// absent, expired, revoked, or the filename or credentials do not match.
func FuzzServePath(f *testing.F) {
	for _, seed := range [][4]string{
		{"GET", "/grants/{open}/open.iso", "", ""},
		{"HEAD", "/grants/{locked}/locked.iso", "bmc", "s3cret"},
		{"GET", "/grants/{locked}/locked.iso", "bmc", "wrong"},
		{"GET", "/grants/{locked}/locked.iso", "", ""},
		{"GET", "/grants/{expired}/expired.iso", "", ""},
		{"GET", "/grants/{revoked}/revoked.iso", "", ""},
		{"GET", "/grants/{open}/locked.iso", "", ""},
		{"GET", "/grants/{open}/../{open}/open.iso", "", ""},
		{"GET", "/grants/{open}/open%2eiso", "", ""},
		{"GET", "/grants/0000/open.iso", "", ""},
		{"POST", "/grants/{open}/open.iso", "", ""},
		{"GET", "/grants/{open}", "", ""},
		{"GET", "/", "", ""},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3])
	}

	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	table := NewGrants()
	table.now = func() time.Time { return now }
	issue := func(name string, ttl time.Duration, creds *Credentials) *Grant {
		g, err := table.Issue(name, name+".iso", []byte("content of "+name), ttl, creds)
		require.NoError(f, err)
		return g
	}
	open := issue("open", time.Hour, nil)
	locked := issue("locked", time.Hour, &Credentials{Username: "bmc", Password: "s3cret"})
	expired := issue("expired", time.Second, nil)
	revoked := issue("revoked", time.Hour, nil)
	table.Revoke(revoked.Token)
	now = now.Add(time.Minute)
	live := map[string]*Grant{open.Token: open, locked.Token: locked}
	srv := &Server{Grants: table}
	tokens := strings.NewReplacer("{open}", open.Token, "{locked}", locked.Token,
		"{expired}", expired.Token, "{revoked}", revoked.Token)

	refused := httptest.NewRecorder()
	refuse(refused)

	f.Fuzz(func(t *testing.T, method, path, user, pass string) {
		req, err := http.NewRequest(method, "http://media"+tokens.Replace(path), nil)
		if err != nil || !strings.HasPrefix(req.URL.Path, "/") {
			return
		}
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)

		rest, isGrant := strings.CutPrefix(req.URL.Path, "/grants/")
		token, filename, wellFormed := strings.Cut(rest, "/")
		g := live[token]
		match := isGrant && wellFormed && g != nil && filename == g.Filename
		credsOK := g == nil || g.Credentials == nil || (user == g.Credentials.Username && pass == g.Credentials.Password)
		getOrHead := req.Method == http.MethodGet || req.Method == http.MethodHead

		switch rec.Code {
		case http.StatusOK, http.StatusPartialContent:
			assert.True(t, getOrHead && match && credsOK, "%s %q answered %d", method, req.URL.Path, rec.Code)
			if req.Method == http.MethodGet && rec.Code == http.StatusOK {
				assert.Equal(t, []byte("content of "+g.Asset), body)
			}
		}
		refusal := getOrHead && isGrant && wellFormed && (!match || !credsOK || (g.Credentials != nil && user == "" && pass == ""))
		if refusal {
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s %q", method, req.URL.Path)
			assert.Equal(t, refused.Header().Get("WWW-Authenticate"), rec.Header().Get("WWW-Authenticate"))
			if req.Method == http.MethodGet {
				assert.Equal(t, refused.Body.String(), string(body), "the refusal must not tell grants apart")
			}
		}
	})
}

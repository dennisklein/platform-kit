package bench

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// spaHandler serves the built web UI. Unknown paths get index.html so client
// routes work; without a build it serves a short fallback page.
func (s *Server) spaHandler() http.Handler {
	var index []byte
	if s.spa != nil {
		index, _ = fs.ReadFile(s.spa, "index.html")
	}
	if index == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(fallbackPage))
		})
	}
	files := http.FileServer(http.FS(s.spa))
	modTime := time.Now()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(s.spa, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", modTime, bytes.NewReader(index))
	})
}

const fallbackPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>platform-kit bench</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}
code{background:#f3f3f3;padding:1px 5px;border-radius:3px}</style></head>
<body><h1>platform-kit bench</h1>
<p>The bench runs, but the web UI is not built into this binary.</p>
<p>Run <code>make web build</code> (or <code>cd web &amp;&amp; npm ci &amp;&amp; npm run build</code> and rebuild <code>pk</code>).
The API is at <code>/api/v1</code>.</p></body></html>
`

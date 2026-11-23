package bench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dennisklein/platform-kit/internal/identity"
)

const keepAlive = 15 * time.Second

// handleEvents streams events as server-sent events. Each message is
// "id: <n>", "event: <type>", "data: <Event JSON>". A comment line every 15 s
// keeps proxies from closing the stream. Last-Event-ID replays missed events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := identity.MustFrom(r.Context())
	rc := http.NewResponseController(w)

	var last uint64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	} else if v := r.URL.Query().Get("lastEventId"); v != "" {
		last, _ = strconv.ParseUint(v, 10, 64)
	}
	ch, cancel := s.hub.Subscribe(id.Principal(), last)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 2000\n: connected as %s\n\n", id.User)
	if err := rc.Flush(); err != nil {
		return
	}

	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				s.log.Error("sse marshal", "type", ev.Type, "err", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		}
	}
}

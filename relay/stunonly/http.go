package stunonly

import (
	"fmt"
	"net/http"
	"sort"
)

type ListenerStatus struct {
	ID     string
	Server *Server
}

func (l ListenerStatus) metrics() Metrics {
	if l.Server == nil {
		return Metrics{}
	}
	return l.Server.Metrics()
}

// Handler exposes independent listener health and bounded-label metrics.
func Handler(listeners []ListenerStatus) http.Handler {
	copied := append([]ListenerStatus(nil), listeners...)
	sort.Slice(copied, func(i, j int) bool { return copied[i].ID < copied[j].ID })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		healthy := len(copied) > 0
		for _, listener := range copied {
			if !listener.metrics().Healthy {
				healthy = false
			}
		}
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		for _, listener := range copied {
			_, _ = fmt.Fprintf(w, "%s healthy=%t\n", listener.ID, listener.metrics().Healthy)
		}
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for _, listener := range copied {
			m := listener.metrics()
			for _, metric := range []struct {
				name  string
				value uint64
			}{
				{"received_total", m.Received}, {"responded_total", m.Responded}, {"invalid_total", m.Invalid},
				{"limited_total", m.Limited}, {"write_errors_total", m.WriteErrors},
			} {
				_, _ = fmt.Fprintf(w, "windshare_stun_%s{listener=%q} %d\n", metric.name, listener.ID, metric.value)
			}
			healthy := 0
			if m.Healthy {
				healthy = 1
			}
			_, _ = fmt.Fprintf(w, "windshare_stun_healthy{listener=%q} %d\n", listener.ID, healthy)
		}
	})
	return mux
}

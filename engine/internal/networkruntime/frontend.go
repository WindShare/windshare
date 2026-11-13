package networkruntime

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
)

const frontendTimeout = 3 * time.Second

func (r *Runtime) frontend(explicit string) (string, string) {
	if explicit != "" {
		return explicit, "explicit"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.LinkHost != "" && (!r.linkUnreachable || !r.backupReachable) {
		return r.config.LinkHost, "builtin_link_host"
	}
	return r.config.BackupEntry, "builtin_backup"
}

func (r *Runtime) run() {
	defer close(r.done)
	ticker := time.NewTicker(networkstate.DefaultPollInterval)
	defer ticker.Stop()
	first := true
	for {
		_, changed, err := r.monitor.Poll(r.ctx, r.now())
		if first || err == nil && changed {
			first = false
			r.checkFrontends()
		}
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runtime) checkFrontends() {
	if r.config.LinkHost == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, frontendTimeout)
	defer cancel()
	check := func(address string) bool {
		if address == "" {
			return false
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodHead, address, nil)
		if err != nil {
			return false
		}
		client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest
	}
	backup := make(chan bool, 1)
	go func() { backup <- check(r.config.BackupEntry) }()
	linkReady, backupReady := check(r.config.LinkHost), <-backup
	r.mu.Lock()
	r.linkUnreachable, r.backupReachable = !linkReady, backupReady
	r.mu.Unlock()
	slog.Debug("network_frontend_health", "generation", r.generation(), "link_host", r.config.LinkHost, "reachable", linkReady, "backup_reachable", backupReady)
}

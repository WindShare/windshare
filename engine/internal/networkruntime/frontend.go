package networkruntime

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/core/diagnostics"
)

const frontendTimeout = 3 * time.Second

type frontendChoice struct {
	primary   string
	alternate string
	reason    string
}

func (r *Runtime) frontend(explicit string) frontendChoice {
	if explicit != "" {
		// Explicit frontends reflect the user's trust choice and must never offer
		// a built-in official origin as an alternate link.
		return frontendChoice{primary: explicit, reason: "explicit"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config.LinkHost != "" && (!r.linkUnreachable || !r.backupReachable) {
		return frontendChoice{
			primary:   r.config.LinkHost,
			alternate: alternateFrontend(r.config.LinkHost, r.config.BackupEntry),
			reason:    "builtin_link_host",
		}
	}
	return frontendChoice{
		primary:   r.config.BackupEntry,
		alternate: alternateFrontend(r.config.BackupEntry, r.config.LinkHost),
		reason:    "builtin_backup",
	}
}

func alternateFrontend(primary, candidate string) string {
	if primary == "" || candidate == "" || sameFrontendBase(primary, candidate) {
		return ""
	}
	return candidate
}

func sameFrontendBase(left, right string) bool {
	first, firstErr := url.Parse(left)
	second, secondErr := url.Parse(right)
	if firstErr != nil || secondErr != nil {
		return left == right
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(first.Host, second.Host) &&
		strings.TrimSuffix(first.Path, "/") == strings.TrimSuffix(second.Path, "/")
}

func (r *Runtime) run() {
	defer close(r.done)
	ticker := time.NewTicker(networkstate.DefaultPollInterval)
	defer ticker.Stop()
	for first := true; ; first = false {
		r.pollNetwork(first)
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runtime) pollNetwork(first bool) {
	_, changed, err := r.monitor.Poll(r.ctx, r.now())
	if !first && (err != nil || !changed) {
		return
	}
	if !first && r.directory != nil {
		r.directory.NetworkChanged()
	}
	r.checkFrontends()
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
	if r.diagnostics.Enabled() {
		fields := diagnostics.Fields{
			"network_generation": diagnostics.Uint(r.generation()),
			"link_host":          diagnostics.Text(r.config.LinkHost),
			"reachable":          diagnostics.Bool(linkReady),
			"backup_reachable":   diagnostics.Bool(backupReady),
		}
		if r.config.BackupEntry != "" {
			fields["backup_entry"] = diagnostics.Text(r.config.BackupEntry)
		}
		r.diagnostics.Emit(diagnostics.New("network_frontend_health", fields).At(r.now()))
	}
}

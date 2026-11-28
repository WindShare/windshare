package turnissuer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/core/urlhost"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/engine/relayconfig"
)

const cloudflareKeys = "https://rtc.live.cloudflare.com/v1/turn/keys/"
const responseLimit = 32 << 10

var errUnavailable = errors.New("TURN credentials unavailable")
var errDisabled = errors.New("managed TURN disabled")

func managedOrigin(address string) (string, bool) {
	u, err := url.Parse(address)
	if err != nil || u.User != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		ip, _ := netip.ParseAddr(u.Hostname())
		if !strings.EqualFold(u.Hostname(), "localhost") && !ip.IsLoopback() {
			return "", false
		}
		u.Scheme = "http"
	default:
		return "", false
	}
	origin, err := urlhost.Origin(u)
	if err != nil {
		return "", false
	}
	return origin, true
}

func (r *Resolver) request(ctx context.Context, method, address, token string, body, out any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, input)
	if err != nil {
		return errUnavailable
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.options.HTTP.Do(req)
	if err != nil {
		return errUnavailable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil || len(data) > responseLimit {
		return errUnavailable
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var e struct{ Error string }
		if resp.StatusCode == http.StatusForbidden && json.Unmarshal(data, &e) == nil && e.Error == "turn_disabled" {
			return errDisabled
		}
		return errUnavailable
	}
	if json.Unmarshal(data, out) != nil {
		return errUnavailable
	}
	return nil
}

func (r *Resolver) issue(ctx context.Context, s source, trace turngrant.Trace) (turngrant.Credential, error) {
	c := turngrant.Credential{ServiceID: s.ID, URLs: slices.Clone(s.URLs)}
	switch s.Kind {
	case relayconfig.TURNStatic:
		c.Username, c.Password = s.Username, s.Password
	case relayconfig.TURNCoturn:
		var nonce [16]byte
		r.randomMu.Lock()
		_, err := io.ReadFull(r.options.Random, nonce[:])
		r.randomMu.Unlock()
		if err != nil {
			return c, err
		}
		// Coturn authenticates the whole Unix second embedded in the username.
		c.ExpiresAt = r.options.Now().Add(time.Duration(s.TTL()) * time.Second).Truncate(time.Second)
		c.Username = strconv.FormatInt(c.ExpiresAt.Unix(), 10) + ":" + hex.EncodeToString(nonce[:])
		// TURN REST authentication specifies HMAC-SHA1 over the complete username.
		mac := hmac.New(sha1.New, []byte(s.SharedSecret))
		mac.Write([]byte(c.Username))
		c.Password = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	case relayconfig.TURNCloudflare:
		var response struct {
			Servers json.RawMessage `json:"iceServers"`
		}
		// The provider starts its TTL during the request. Its response and the
		// rest of this batch can arrive later.
		c.ExpiresAt = r.options.Now().Add(time.Duration(s.TTL()) * time.Second)
		err := r.request(ctx, http.MethodPost, cloudflareKeys+url.PathEscape(s.KeyID)+"/credentials/generate-ice-servers", s.APIToken, map[string]int{"ttl": s.TTL()}, &response)
		if err != nil {
			return c, err
		}
		selected, err := selectCloudflareServers(response.Servers, s.URLs)
		if err != nil {
			return c, err
		}
		c.URLs, c.Username, c.Password = selected.URLs, selected.Username, selected.Credential
	case "managed":
		var response struct {
			URLs               []string
			Username, Password string
			TTL                int `json:"ttl_seconds"`
		}
		requestedAt := r.options.Now()
		err := r.request(ctx, http.MethodPost, s.origin+"/v2/turn/credentials", s.access, nil, &response)
		if errors.Is(err, errDisabled) {
			r.mu.Lock()
			r.known[s.ID] = false
			r.mu.Unlock()
			trace.Event("turn_managed_disabled", s.ID, nil)
		}
		if err != nil || response.TTL <= 0 || response.TTL > relayconfig.MaximumTURNTTL {
			return c, errUnavailable
		}
		c.URLs, c.Username, c.Password = response.URLs, response.Username, response.Password
		c.ExpiresAt = requestedAt.Add(time.Duration(response.TTL) * time.Second)
	default:
		return c, errUnavailable
	}
	// Validate remote responses before retaining or sending any credential.
	check := p.TURNGrant{ServiceID: c.ServiceID, GrantID: p.TURNGrantID{1}, URLs: c.URLs, Username: c.Username, Password: c.Password}
	if !check.Valid() {
		return c, errUnavailable
	}
	return c, nil
}

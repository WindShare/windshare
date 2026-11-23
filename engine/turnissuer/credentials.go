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
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/windshare/windshare/connectivity/turnsession"
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
	switch u.Scheme {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		ip, _ := netip.ParseAddr(u.Hostname())
		if u.Hostname() != "localhost" && !ip.IsLoopback() {
			return "", false
		}
		u.Scheme = "http"
	default:
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
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

func (r *Resolver) issue(ctx context.Context, s source) (turnsession.Credential, error) {
	c := turnsession.Credential{ServiceID: s.ID, URLs: slices.Clone(s.URLs), ValidFor: time.Duration(s.TTL()) * time.Second}
	switch s.Kind {
	case relayconfig.TURNStatic:
		c.Username, c.Password, c.ValidFor = s.Username, s.Password, 0
	case relayconfig.TURNCoturn:
		var nonce [16]byte
		r.randomMu.Lock()
		_, err := io.ReadFull(r.options.Random, nonce[:])
		r.randomMu.Unlock()
		if err != nil {
			return c, err
		}
		c.Username = strconv.FormatInt(r.options.Now().Add(c.ValidFor).Unix(), 10) + ":" + hex.EncodeToString(nonce[:])
		// TURN REST authentication specifies HMAC-SHA1 over the complete username.
		mac := hmac.New(sha1.New, []byte(s.SharedSecret))
		mac.Write([]byte(c.Username))
		c.Password = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	case relayconfig.TURNCloudflare:
		var response struct {
			Servers json.RawMessage `json:"iceServers"`
		}
		err := r.request(ctx, http.MethodPost, cloudflareKeys+url.PathEscape(s.KeyID)+"/credentials/generate-ice-servers", s.APIToken, map[string]int{"ttl": s.TTL()}, &response)
		if err != nil {
			return c, err
		}
		if err := selectCloudflareServers(&c, response.Servers, s.URLs); err != nil {
			return c, err
		}
	case "managed":
		var response struct {
			URLs               []string
			Username, Password string
			TTL                int `json:"ttl_seconds"`
		}
		err := r.request(ctx, http.MethodPost, s.origin+"/v2/turn/credentials", s.access, nil, &response)
		if errors.Is(err, errDisabled) {
			r.mu.Lock()
			r.known[s.ID] = false
			r.mu.Unlock()
			slog.Debug("turn_managed_disabled", "service_id", s.ID)
		}
		if err != nil || response.TTL <= 0 || response.TTL > relayconfig.MaximumTURNTTL {
			return c, errUnavailable
		}
		c.URLs, c.Username, c.Password, c.ValidFor = response.URLs, response.Username, response.Password, time.Duration(response.TTL)*time.Second
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

func selectCloudflareServers(c *turnsession.Credential, response json.RawMessage, filters []string) error {
	type server struct {
		URLs                 []string
		Username, Credential string
	}
	var servers []server
	if json.Unmarshal(response, &servers) != nil {
		var one server
		if json.Unmarshal(response, &one) != nil {
			return errUnavailable
		}
		servers = []server{one}
	}
	c.URLs = nil
	for _, entry := range servers {
		if entry.Username == "" || entry.Credential == "" {
			continue
		}
		if c.Username != "" && (entry.Username != c.Username || entry.Credential != c.Password) {
			continue
		}
		for _, u := range entry.URLs {
			if p.ValidTURNURL(u) && (len(filters) == 0 || slices.ContainsFunc(filters, func(filter string) bool { return u == filter || strings.HasPrefix(u, filter+"?") })) && !slices.Contains(c.URLs, u) {
				c.Username, c.Password = entry.Username, entry.Credential
				c.URLs = append(c.URLs, u)
			}
		}
	}
	return nil
}

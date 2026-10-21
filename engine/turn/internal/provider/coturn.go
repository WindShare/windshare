package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // coturn's TURN REST credential format specifies HMAC-SHA1.
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

type CoturnConfig struct {
	SharedSecret string
	Lifetime     time.Duration
	Now          func() time.Time
}

type Coturn struct{ config CoturnConfig }

func (config CoturnConfig) Validate() error {
	if config.Lifetime == 0 {
		config.Lifetime = DefaultLifetime
	}
	if config.SharedSecret == "" || !validLifetime(config.Lifetime) {
		return ErrConfiguration
	}
	return nil
}

func NewCoturn(config CoturnConfig) (*Coturn, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Lifetime == 0 {
		config.Lifetime = DefaultLifetime
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Coturn{config}, nil
}

func (source *Coturn) Issue(ctx context.Context, subject Subject) (Issued, error) {
	if err := ctx.Err(); err != nil {
		return Issued{}, err
	}
	if subject.SessionID == ([16]byte{}) || subject.PathID == ([16]byte{}) {
		return Issued{}, ErrConfiguration
	}
	expires := source.config.Now().Add(source.config.Lifetime).Truncate(time.Second)
	username := strconv.FormatInt(expires.Unix(), 10) + ":" +
		hex.EncodeToString(subject.SessionID[:]) + ":" + hex.EncodeToString(subject.PathID[:])
	mac := hmac.New(sha1.New, []byte(source.config.SharedSecret))
	_, _ = mac.Write([]byte(username))
	credential, err := iceconfig.Expiring(username, base64.StdEncoding.EncodeToString(mac.Sum(nil)), expires)
	return Issued{Credentials: credential}, err
}

func (source *Coturn) Renew(ctx context.Context, subject Subject, _ Issued) (Issued, error) {
	return source.Issue(ctx, subject)
}

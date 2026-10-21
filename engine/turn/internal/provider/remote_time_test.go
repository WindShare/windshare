package provider

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestRemoteExpiryUsesIssuerClockAndChargesResponseTime(t *testing.T) {
	for _, offset := range []time.Duration{0, 2 * time.Second, 24 * time.Hour, -24 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			start := time.Unix(1700000000, 750000000)
			now := start
			const latency = 400 * time.Millisecond
			serverExpires := start.Add(offset + time.Hour)
			source, err := NewRemote(RemoteConfig{
				RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
				Now: func() time.Time { return now },
				Client: clientFunc(func(request *http.Request) (*http.Response, error) {
					now = now.Add(latency)
					return remoteTimedResponse(t, request, serverExpires.UnixMilli(), now.Add(offset).UTC().Format(http.TimeFormat)), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
			if err != nil {
				t.Fatal(err)
			}
			// The Date header can lag response generation by almost a second.
			// Neither that uncertainty nor the response trip may extend authority.
			serverUpperBound := now.Add(offset).Truncate(time.Second).Add(time.Second)
			want := start.Add(serverExpires.Sub(serverUpperBound))
			if got := acquired.Credentials.ExpiresAt(); !got.Equal(want) || !acquired.Credentials.ValidAt(now) {
				t.Fatalf("local expiry = %v, want %v (now %v)", got, want, now)
			}
		})
	}
}

func TestRemoteExpiryRejectsInvalidTimingWithoutLosingCleanup(t *testing.T) {
	serverNow := time.Unix(1700000000, 0)
	for _, test := range []struct {
		name    string
		date    string
		expires int64
		elapsed time.Duration
		want    error
	}{
		{name: "missing server clock", expires: serverNow.Add(time.Hour).UnixMilli(), want: ErrConfiguration},
		{name: "invalid server clock", date: "invalid", expires: serverNow.Add(time.Hour).UnixMilli(), want: ErrConfiguration},
		{name: "expired", date: serverNow.UTC().Format(http.TimeFormat), expires: serverNow.UnixMilli(), want: ErrExpired},
		{name: "date uncertainty exhausts lease", date: serverNow.UTC().Format(http.TimeFormat), expires: serverNow.Add(time.Second).UnixMilli(), want: ErrExpired},
		{name: "response time exhausts lease", date: serverNow.UTC().Format(http.TimeFormat), expires: serverNow.Add(3 * time.Second).UnixMilli(), elapsed: 2 * time.Second, want: ErrExpired},
		{name: "excessive lifetime", date: serverNow.UTC().Format(http.TimeFormat), expires: serverNow.Add(time.Hour + time.Second + time.Millisecond).UnixMilli(), want: ErrConfiguration},
		{name: "overflowing lifetime", date: serverNow.UTC().Format(http.TimeFormat), expires: math.MaxInt64, want: ErrConfiguration},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := serverNow.Add(-24 * time.Hour)
			source, err := NewRemote(RemoteConfig{
				RelayURL: "https://relay.example", AccessKey: "key", ServiceID: "cf-service",
				Now: func() time.Time { return now },
				Client: clientFunc(func(request *http.Request) (*http.Response, error) {
					now = now.Add(test.elapsed)
					return remoteTimedResponse(t, request, test.expires, test.date), nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			acquired, err := source.Issue(t.Context(), Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if acquired.Cleanup == nil || acquired.Credentials.Kind() != 0 {
				t.Fatal("invalid timing lost cleanup ownership or exposed credentials")
			}
		})
	}
}

func remoteTimedResponse(t *testing.T, request *http.Request, expires int64, date string) *http.Response {
	t.Helper()
	var body remoteRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(remoteGrant{
		ServiceID: body.ServiceID, LeaseID: body.LeaseID, SessionID: body.SessionID, PathID: body.PathID,
		URLs: []string{"turn:localhost:3478"}, Username: "user", Password: "password", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response(http.StatusCreated, string(data))
	result.Header = http.Header{"Date": {date}}
	return result
}

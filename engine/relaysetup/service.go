// Package relaysetup implements discover, approve and install for native applications.
package relaysetup

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turnprovider"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Operation string

const (
	AddProfile        Operation = "add"
	PrivateOnboarding Operation = "private-onboarding"
)

var (
	ErrDenied          = errors.New("relay access was refused; approve this device or install a new access key")
	ErrExpired         = errors.New("relay enrollment expired; start setup again")
	ErrPendingSetup    = errors.New("another relay setup is pending; finish or cancel it first")
	ErrSetupRunning    = errors.New("relay setup is already running; finish or cancel it first")
	ErrSettingsChanged = errors.New("sharing settings changed during relay setup; cancel this setup and start again")
	ErrSetupSuperseded = errors.New("relay setup was completed, cancelled or replaced")
)

type Store interface {
	Load() (Snapshot, error)
	Update(context.Context, func(*Snapshot) error) (Snapshot, error)
}
type DiscoveryCache interface {
	Save(context.Context, senderrelay.Candidate, turnprovider.Capability, time.Time) error
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type Dependencies struct {
	Store       Store
	Cache       DiscoveryCache
	HTTP        HTTPClient
	OpenBrowser func(context.Context, string) error
	Now         func() time.Time
	Random      io.Reader
	Wait        func(context.Context, time.Duration) error
	Log         *slog.Logger
}
type Request struct {
	URL, DeviceName string
	Operation       Operation
	NoBrowser       bool
}
type Result struct {
	RelayURL, CredentialID, FrontURL string
	ActiveRelays                     []string
}
type Progress struct {
	Stage, OperationID, EnrollmentID, VerificationURL, UserCode string
	RetryAfter                                                  time.Duration
}

func (Progress) EngineEvent() {}

type Service struct {
	dependencies Dependencies
	mu           sync.Mutex
	active       *activity
	cancelling   bool
}

func New(dependencies Dependencies) *Service {
	if dependencies.HTTP == nil {
		dependencies.HTTP = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.Random == nil {
		dependencies.Random = rand.Reader
	}
	if dependencies.Wait == nil {
		dependencies.Wait = wait
	}
	if dependencies.Log == nil {
		dependencies.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Service{dependencies: dependencies}
}
func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (service *Service) emit(progress func(Progress), event Progress) {
	service.dependencies.Log.Info("relay_setup", "stage", event.Stage, "operation_id", event.OperationID,
		"enrollment_id", event.EnrollmentID, "retry_after", event.RetryAfter)
	if progress != nil {
		progress(event)
	}
}
func (service *Service) Run(ctx context.Context, request Request, progress func(Progress)) (Result, error) {
	ctx, finish, err := service.begin(ctx, request.URL)
	if err != nil {
		return Result{}, err
	}
	defer finish()
	if service.dependencies.Store == nil {
		return Result{}, errors.New("relay configuration storage is unavailable")
	}
	if request.Operation != AddProfile && request.Operation != PrivateOnboarding {
		return Result{}, errors.New("unknown relay setup operation")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	saved, err := service.dependencies.Store.Load()
	if err != nil {
		return Result{}, err
	}
	if saved.Pending != nil {
		if !sameRelay(saved.Pending.RelayURL, request.URL) || saved.Pending.Operation != request.Operation {
			return Result{}, ErrPendingSetup
		}
		return service.continueEnrollment(ctx, saved, progress, request.NoBrowser)
	}
	discovery, err := service.discover(ctx, request.URL)
	if err != nil {
		return Result{}, err
	}
	if request.Operation == PrivateOnboarding && discovery.Access != "restricted" {
		return Result{}, errors.New("private onboarding requires a restricted relay")
	}
	if discovery.Access == "open" {
		return service.install(ctx, saved.SettingsRevision(), discovery.RelayURL, "", "", discovery.FrontendURL, request.Operation)
	}
	if request.DeviceName == "" {
		return Result{}, errors.New("device name is required")
	}
	secret := make([]byte, claimSecretBytes)
	if _, err := io.ReadFull(service.dependencies.Random, secret); err != nil {
		return Result{}, err
	}
	operation := make([]byte, operationIDBytes)
	if _, err := io.ReadFull(service.dependencies.Random, operation); err != nil {
		return Result{}, err
	}
	saved.Pending = &PendingSetup{setupIntent: setupIntent{
		SettingsRevision: saved.SettingsRevision(), OperationID: encode(operation),
		RelayURL: discovery.RelayURL, ConsoleURL: discovery.ConsoleURL, FrontURL: discovery.FrontendURL,
		DeviceName: request.DeviceName, ClaimSecret: encode(secret), Operation: request.Operation,
	}, phase: requesting{}}

	// Persist the claimant before the first request; a lost response can recover by digest.
	pending := saved.Pending
	saved, err = service.dependencies.Store.Update(ctx, func(current *Snapshot) error {
		if current.Pending != nil {
			return ErrPendingSetup
		}
		if current.SettingsRevision() != pending.SettingsRevision {
			return ErrSettingsChanged
		}
		// Detect capacity and selection failures before asking anyone to approve.
		if _, err := installation(current.Config, pending.RelayURL, "", pending.FrontURL, request.Operation); err != nil {
			return err
		}
		current.Pending = pending
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return service.continueEnrollment(ctx, saved, progress, request.NoBrowser)
}

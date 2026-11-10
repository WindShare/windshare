// Package relaysetup implements discover, approve and install for native applications.
package relaysetup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
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
	ErrNoPendingSetup  = errors.New("no relay setup is pending; nothing was cancelled")
	ErrSetupRunning    = errors.New("relay setup is already running; finish or cancel it first")
	ErrSettingsChanged = errors.New("sharing settings changed during relay setup")
	ErrSetupSuperseded = errors.New("relay setup was completed, cancelled or replaced")
)

// The recovery for ErrSettingsChanged depends on whether a pending record
// exists: before one is saved there is nothing to cancel, and once one is saved
// it blocks every new setup until it is cancelled.
var (
	errSettingsChangedBeforePending = fmt.Errorf("%w; start again", ErrSettingsChanged)
	errSettingsChangedWhilePending  = fmt.Errorf("%w; cancel this setup and start again", ErrSettingsChanged)
	errFinishedBeforeCancel         = fmt.Errorf("relay setup finished before it could be cancelled; %w", ErrNoPendingSetup)
)

type Store interface {
	Load() (Snapshot, error)
	Update(context.Context, func(*Snapshot) error) (Snapshot, error)
}
type DiscoveryCache interface {
	Save(context.Context, relayprofile.Candidate, turn.Capability, time.Time) error
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
	workflow
	mu         sync.Mutex
	active     *activity
	cancelling bool
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
func (service *Service) Run(ctx context.Context, request Request, progress func(Progress)) (result Result, resultErr error) {
	ctx, finish, err := service.begin(ctx, request.URL)
	if err != nil {
		return Result{}, err
	}
	defer func() { finish(resultErr == nil) }()
	if service.dependencies.Store == nil {
		return Result{}, errors.New("relay configuration storage is unavailable")
	}
	store := &recoveryStore{Store: service.dependencies.Store, request: request,
		recovery: Recovery{Action: RecoveryUnknown, RelayURL: request.URL}}
	dependencies := service.dependencies
	dependencies.Store = store
	run := workflow{dependencies: dependencies}
	result, resultErr = run.execute(ctx, request, progress)
	if resultErr != nil {
		service.dependencies.Log.Info("relay_setup_failed", "operation", request.Operation,
			"operation_id", store.recovery.OperationID, "relay_url", store.recovery.RelayURL,
			"recovery", store.recovery.Action, "reason", resultErr)
		resultErr = &SetupError{Recovery: store.recovery, cause: resultErr}
	}
	return result, resultErr
}

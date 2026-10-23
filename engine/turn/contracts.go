// Package turn exposes application settings, status and observations.
// Native leases, account ownership and discovery assembly belong to the engine.
package turn

import "github.com/windshare/windshare/engine/internal/turnruntime"

type Settings = turnruntime.Settings
type ManagedSettings = turnruntime.ManagedSettings
type ManagedPolicy = turnruntime.ManagedPolicy
type ManualSettings = turnruntime.ManualSettings
type AccountSettings = turnruntime.AccountSettings
type CloudflareAnalytics = turnruntime.CloudflareAnalytics
type ProfileSettings = turnruntime.ProfileSettings
type StaticCredentials = turnruntime.StaticCredentials
type CloudflareCredentials = turnruntime.CloudflareCredentials
type CoturnCredentials = turnruntime.CoturnCredentials
type Capability = turnruntime.Capability
type Cache = turnruntime.Cache
type HTTPClient = turnruntime.HTTPClient
type Dependencies = turnruntime.Dependencies
type Usage = turnruntime.Usage
type ServiceIdentity = turnruntime.ServiceIdentity
type SessionSubject = turnruntime.SessionSubject
type TrafficUsage = turnruntime.TrafficUsage
type Event = turnruntime.Event
type Status = turnruntime.Status

const (
	DelegationNotice       = turnruntime.DelegationNotice
	StaticIssuer           = turnruntime.StaticIssuer
	CloudflareIssuer       = turnruntime.CloudflareIssuer
	CoturnIssuer           = turnruntime.CoturnIssuer
	MaximumProfiles        = turnruntime.MaximumProfiles
	MaximumAccounts        = turnruntime.MaximumAccounts
	MaximumManagedServices = turnruntime.MaximumManagedServices
)

var (
	ErrConfiguration = turnruntime.ErrConfiguration
	ErrBudget        = turnruntime.ErrBudget
	ErrCapacity      = turnruntime.ErrCapacity
	ErrClosed        = turnruntime.ErrClosed
)

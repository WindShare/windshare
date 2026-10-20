// SPDX-FileCopyrightText: 2026 WindShare contributors
// SPDX-License-Identifier: MIT

package turn

import "github.com/pion/turn/v5/internal/client"

type BindingStatus = client.BindingStatus
type BindingEvent = client.BindingEvent

const (
	BindingActive    = client.BindingActive
	BindingRetrying  = client.BindingRetrying
	BindingRejected  = client.BindingRejected
	BindingMismatch  = client.BindingMismatch
	BindingExpired   = client.BindingExpired
	BindingExhausted = client.BindingExhausted
)

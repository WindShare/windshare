package relayset

type routeRegistration struct {
	state    RegistrationState
	terminal bool
}

// SenderAvailability describes only admission through relays. Existing direct
// transfers have independent lifetimes and must never be inferred from this count.
type SenderAvailability struct {
	Available uint32
	Total     uint32
	EverReady bool
	Terminal  uint32
}

type SenderStatus struct {
	Availability          SenderAvailability
	AuthorizationRequired bool
}

// Status reads the owner's current state even when observations were dropped.
func (s *Sender) Status() SenderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Sender) statusLocked() SenderStatus {
	availability := SenderAvailability{Total: uint32(len(s.routes)), EverReady: s.everReady}
	var blocked uint32
	for _, route := range s.routes {
		switch {
		case route.terminal:
			availability.Terminal++
		case route.state == RegistrationAvailable:
			availability.Available++
		case route.state == RegistrationNeedsUserAuthorization:
			blocked++
		}
	}
	// A pending alternative can still publish without user action. Once a route
	// admitted the share, its outages belong to recovery rather than startup.
	return SenderStatus{Availability: availability, AuthorizationRequired: !availability.EverReady &&
		blocked > 0 && blocked+availability.Terminal == availability.Total}
}

func (s *Sender) setRegistration(url string, registration routeRegistration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.routes[url]; current == registration || current.terminal {
		return
	}
	s.routes[url] = registration
	s.everReady = s.everReady || registration.state == RegistrationAvailable
	if !s.stopping && s.ctx.Err() == nil && s.observer != nil {
		s.observer(s.statusLocked())
	}
}

func (s *Sender) ObserveStatus(observer func(SenderStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observer = observer
	if observer != nil {
		observer(s.statusLocked())
	}
}

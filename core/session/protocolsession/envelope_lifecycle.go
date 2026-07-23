package protocolsession

// Stop freezes future envelope use without touching fields an in-flight writer
// or pump may still be reading. The lane owner calls Destroy only after those
// components have joined.
func (s *EnvelopeSealer) Stop() {
	if s != nil {
		s.stopped.Store(true)
	}
}

// Destroy releases every reference and zeroes all directly accessible
// cryptographic state owned by the sealer. Go's cipher.AEAD does not expose a
// destructor for its internal AES expanded schedule; clearing aead drops this
// component's only reference so that opaque schedule becomes garbage-collector
// managed rather than pretending it was deterministically overwritten.
func (s *EnvelopeSealer) Destroy() {
	if s == nil {
		return
	}
	s.Stop()
	s.aead = nil
	s.binding = EnvelopeBinding{}
	s.nonceSource = nil
	clear(s.nonce[:])
	s.nonceReady = false
	s.next = 0
	s.exhausted = false
}

func (s *EnvelopeSealer) Close() {
	s.Destroy()
}

// Stop is safe at the protocol callback boundary; Destroy remains the lane
// owner's post-pump join operation.
func (o *EnvelopeOpener) Stop() {
	if o != nil {
		o.stopped.Store(true)
	}
}

// Destroy drops the opener's sole AEAD reference and zeroes its visible binding
// and sequence state. The standard-library AEAD schedule has no zeroization API
// and is therefore released to GC after this reference is removed.
func (o *EnvelopeOpener) Destroy() {
	if o == nil {
		return
	}
	o.Stop()
	o.aead = nil
	o.binding = EnvelopeBinding{}
	o.next = 0
	o.exhausted = false
}

func (o *EnvelopeOpener) Close() {
	o.Destroy()
}

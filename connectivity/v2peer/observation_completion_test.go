package v2peer

import (
	"errors"
	"runtime"
	"sync"
	"testing"
)

func TestObservationSourceNoReaderSaturationAndIdempotentCompletion(t *testing.T) {
	source, err := newObservationSource[int](2)
	if err != nil {
		t.Fatal(err)
	}
	if source.publish(1) != observationPublishEnqueued ||
		source.publish(2) != observationPublishEnqueued ||
		source.publish(3) != observationPublishCapacityDropped {
		t.Fatal("bounded publication results did not preserve the exact admission facts")
	}

	want := ObservationCompletion{Enqueued: 2, Loss: ObservationLoss{CapacityDropped: 1}}
	if want.Loss.Total() != 1 {
		t.Fatalf("capacity loss total = %d", want.Loss.Total())
	}
	if got := source.completeObservations(); got != want {
		t.Fatalf("completion = %#v, want %#v", got, want)
	}
	if got := source.completeObservations(); got != want {
		t.Fatalf("repeated completion = %#v, want cached %#v", got, want)
	}
	if got := source.publish(4); got != observationPublishAfterCompletion {
		t.Fatalf("publish after completion = %v", got)
	}

	var retained []int
	for value := range source.observations() {
		retained = append(retained, value)
	}
	if len(retained) != 2 || retained[0] != 1 || retained[1] != 2 {
		t.Fatalf("retained FIFO = %#v", retained)
	}
}

func TestObservationSourceConcurrentPublicationAndCompletion(t *testing.T) {
	const publishers = 64
	source, err := newObservationSource[int](publishers)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var work sync.WaitGroup
	for value := range publishers {
		work.Go(func() {
			<-start
			source.publish(value)
		})
	}
	close(start)
	cut := source.completeObservations()
	work.Wait()
	if cut.Enqueued+cut.Loss.CapacityDropped > publishers {
		t.Fatalf("completion counted more publications than existed: %#v", cut)
	}
	for range source.observations() {
	}
}

func TestEnabledFactoriesCreateNoObservationWorkers(t *testing.T) {
	const factories = 200
	runtime.GC()
	before := runtime.NumGoroutine()
	senders := make([]*Factory, 0, factories)
	receivers := make([]*ReceiverFactory, 0, factories)
	for range factories {
		sender, err := NewFactory(Config{
			SenderAttemptObservationCapacity:  1,
			PeerDiagnosticObservationCapacity: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		receiver, err := NewReceiverFactory(ReceiverFactoryConfig{
			ReceiverTerminationObservationCapacity: 1,
			PeerDiagnosticObservationCapacity:      1,
		})
		if err != nil {
			t.Fatal(err)
		}
		senders = append(senders, sender)
		receivers = append(receivers, receiver)
	}
	runtime.Gosched()
	if added := runtime.NumGoroutine() - before; added > 8 {
		t.Fatalf("factory construction started observation workers: goroutine delta = %d", added)
	}
	for _, factory := range senders {
		factory.CompleteObservations()
	}
	for _, factory := range receivers {
		factory.CompleteObservations()
	}
}

func TestDisabledFactoriesExposeNoStreams(t *testing.T) {
	sender, err := NewFactory(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if sender.SenderAttemptObservations() != nil || sender.PeerDiagnostics() != nil {
		t.Fatal("default sender factory activated an observation stream")
	}
	recorder := newSenderAttemptRecorder(sender, newTestPeerSession(220).sessionID, testBinding(221))
	recorder.begin()
	if completion := sender.CompleteObservations(); completion != (SenderObservationCompletion{}) {
		t.Fatalf("disabled sender completion = %#v", completion)
	}

	receiver, err := NewReceiverFactory(ReceiverFactoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if receiver.ReceiverTerminationObservations() != nil || receiver.PeerDiagnostics() != nil {
		t.Fatal("default receiver factory activated an observation stream")
	}
	(&ReceiverAttempt{factory: receiver}).emitTerminationTrace(ReceiverTerminationTrace{})
	if completion := receiver.CompleteObservations(); completion != (ReceiverObservationCompletion{}) {
		t.Fatalf("disabled receiver completion = %#v", completion)
	}
}

func TestFactoriesRejectInvalidObservationCapacities(t *testing.T) {
	for name, config := range map[string]Config{
		"negative sender": {SenderAttemptObservationCapacity: -1},
		"large sender":    {SenderAttemptObservationCapacity: maximumSenderAttemptObservationCapacity + 1},
		"negative peer":   {PeerDiagnosticObservationCapacity: -1},
		"large peer":      {PeerDiagnosticObservationCapacity: maximumPeerDiagnosticObservationCapacity + 1},
	} {
		t.Run("sender "+name, func(t *testing.T) {
			if _, err := NewFactory(config); !errors.Is(err, ErrConfig) {
				t.Fatalf("NewFactory error = %v", err)
			}
		})
	}
	for name, config := range map[string]ReceiverFactoryConfig{
		"negative termination": {ReceiverTerminationObservationCapacity: -1},
		"large termination": {
			ReceiverTerminationObservationCapacity: maximumReceiverTerminationObservationCapacity + 1,
		},
		"negative peer": {PeerDiagnosticObservationCapacity: -1},
		"large peer":    {PeerDiagnosticObservationCapacity: maximumPeerDiagnosticObservationCapacity + 1},
	} {
		t.Run("receiver "+name, func(t *testing.T) {
			if _, err := NewReceiverFactory(config); !errors.Is(err, ErrConfig) {
				t.Fatalf("NewReceiverFactory error = %v", err)
			}
		})
	}
}

func TestSenderAttemptShutdownConcurrentWithCompletionHasStableCut(t *testing.T) {
	factory, err := NewFactory(Config{
		SenderAttemptObservationCapacity:  8,
		PeerDiagnosticObservationCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := newSenderAttemptRecorder(factory, newTestPeerSession(222).sessionID, testBinding(223))
	start := make(chan struct{})
	var completion SenderObservationCompletion
	var work sync.WaitGroup
	work.Add(2)
	go func() {
		defer work.Done()
		<-start
		recorder.begin()
		recorder.fail(SenderAttemptFailure{
			Scope: AttemptFailureScopeSession, TypedPeerErrorCode: TypedPeerErrorStopped,
		})
	}()
	go func() {
		defer work.Done()
		<-start
		completion = factory.CompleteObservations()
	}()
	close(start)
	work.Wait()
	if repeated := factory.CompleteObservations(); repeated != completion {
		t.Fatalf("repeated completion = %#v, want stable %#v", repeated, completion)
	}
	for range factory.SenderAttemptObservations() {
	}
	for range factory.PeerDiagnostics() {
	}
}

func TestSenderCompletionPublishesFinalStreamCapacityDiagnostic(t *testing.T) {
	factory, err := NewFactory(Config{
		SenderAttemptObservationCapacity:  1,
		PeerDiagnosticObservationCapacity: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.observeSenderAttempt(SenderAttemptObservation{Stage: SenderAttemptStarted})
	factory.observeSenderAttempt(SenderAttemptObservation{Stage: SenderAttemptOfferReceived})

	completion := factory.CompleteObservations()
	if completion.Attempts != (ObservationCompletion{Enqueued: 1, Loss: ObservationLoss{CapacityDropped: 1}}) {
		t.Fatalf("attempt completion = %#v", completion.Attempts)
	}
	if completion.Diagnostics != (ObservationCompletion{Enqueued: 1}) {
		t.Fatalf("diagnostic completion = %#v", completion.Diagnostics)
	}
	diagnostic := <-factory.PeerDiagnostics()
	if diagnostic != (PeerDiagnosticObservation{
		Category: PeerDiagnosticSenderAttempt,
		Reason:   PeerDiagnosticStreamCapacity,
		Count:    1,
	}) {
		t.Fatalf("capacity diagnostic = %#v", diagnostic)
	}
	if _, open := <-factory.PeerDiagnostics(); open {
		t.Fatal("diagnostic stream remained open after completion")
	}
	if repeated := factory.CompleteObservations(); repeated != completion {
		t.Fatalf("repeated completion = %#v, want %#v", repeated, completion)
	}
}

func TestReceiverCompletionPublishesFinalStreamCapacityDiagnostic(t *testing.T) {
	factory, err := NewReceiverFactory(ReceiverFactoryConfig{
		ReceiverTerminationObservationCapacity: 1,
		PeerDiagnosticObservationCapacity:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.terminations.publish(ReceiverTerminationTrace{localGeneration: 1})
	factory.terminations.publish(ReceiverTerminationTrace{localGeneration: 2})

	completion := factory.CompleteObservations()
	if completion.Terminations != (ObservationCompletion{Enqueued: 1, Loss: ObservationLoss{CapacityDropped: 1}}) {
		t.Fatalf("termination completion = %#v", completion.Terminations)
	}
	diagnostic := <-factory.PeerDiagnostics()
	if diagnostic.Category != PeerDiagnosticReceiverTermination ||
		diagnostic.Reason != PeerDiagnosticStreamCapacity || diagnostic.Count != 1 {
		t.Fatalf("capacity diagnostic = %#v", diagnostic)
	}
	if _, open := <-factory.ReceiverTerminationObservations(); !open {
		t.Fatal("termination stream omitted its retained observation")
	}
	if _, open := <-factory.ReceiverTerminationObservations(); open {
		t.Fatal("termination stream remained open after its retained observation")
	}
}

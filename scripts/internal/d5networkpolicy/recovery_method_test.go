// These tests drive a deterministic whole-repo SSA static-analysis gate with
// no concurrency of its own: race instrumentation adds ~4x runtime and zero
// signal, so race builds skip them. The non-race coverage gates remain the
// authoritative execution.
//go:build !race

package main

import (
	"strings"
	"testing"
)

func TestBoundMethodRecoveryOutcomeTransformers(t *testing.T) {
	t.Parallel()
	root := writeFixtureModule(t, map[string]string{
		"owner/owner.go": `package owner
import "net"
type Open func()
type Recovery interface { Recover() }
type valueRecoverer struct{}
type pointerRecoverer struct{}
type noRecoverer struct{}
type conditionalRecoverer struct { enabled bool }
type repanicRecoverer struct{}
type nestedRecoverer struct{}
type embeddedRecoverer struct { valueRecoverer }
type embeddedPointerRecoverer struct { *pointerRecoverer }
type genericRecoverer[T any] struct{}
func Socket() { listener, _ := net.Listen("tcp", "127.0.0.1:0"); if listener != nil { _ = listener.Close() } }
func (valueRecoverer) Recover() { _ = recover() }
func (*pointerRecoverer) Recover() { _ = recover() }
func (noRecoverer) Recover() {}
func (receiver conditionalRecoverer) Recover() { if receiver.enabled { _ = recover() } }
func (repanicRecoverer) Recover() { _ = recover(); panic("replacement panic") }
func nestedMethodHelper() { _ = recover() }
func (nestedRecoverer) Recover() { nestedMethodHelper() }
func (genericRecoverer[T]) Recover() { _ = recover() }
func recoveredByBoundValueMethod() {
    deferred := valueRecoverer{}.Recover
    defer deferred()
    panic("value method")
}
func recoveredByBoundPointerMethod() {
    receiver := &pointerRecoverer{}
    deferred := receiver.Recover
    defer deferred()
    panic("pointer method")
}
func recoveredByNilPointerMethod() {
    var receiver *pointerRecoverer
    deferred := receiver.Recover
    defer deferred()
    panic("nil pointer method")
}
func recoveredByBoundInterfaceMethod() {
    var receiver Recovery = valueRecoverer{}
    deferred := receiver.Recover
    defer deferred()
    panic("interface method")
}
func recoveredByPromotedMethod() {
    deferred := (embeddedRecoverer{}).Recover
    defer deferred()
    panic("promoted method")
}
func recoveredByPromotedPointerMethod() {
    receiver := embeddedPointerRecoverer{pointerRecoverer: &pointerRecoverer{}}
    deferred := receiver.Recover
    defer deferred()
    panic("promoted pointer method")
}
func recoveredByGenericBoundMethod() {
    deferred := (genericRecoverer[int]{}).Recover
    defer deferred()
    panic("generic bound method")
}
func recoveredByGenericMethodExpression() {
    deferred := genericRecoverer[int].Recover
    defer deferred(genericRecoverer[int]{})
    panic("generic method expression")
}
func conditionalBoundMethodRecovery(enabled bool) {
    deferred := (conditionalRecoverer{enabled: enabled}).Recover
    defer deferred()
    panic("conditional method")
}
func boundMethodRecoveryRepanics() {
    deferred := repanicRecoverer{}.Recover
    defer deferred()
    panic("original panic")
}
func normalBoundMethodInvocationCannotRecover() {
    deferred := valueRecoverer{}.Recover
    defer func() { deferred() }()
    panic("ordinary method call")
}
func nestedMethodRecoveryCannotRecover() {
    deferred := nestedRecoverer{}.Recover
    defer deferred()
    panic("nested helper")
}
func ambiguousBoundMethodRecovery(enabled bool) {
    var receiver Recovery = noRecoverer{}
    if enabled { receiver = valueRecoverer{} }
    deferred := receiver.Recover
    defer deferred()
    panic("ambiguous method set")
}
func unknownBoundMethodRecovery(receiver Recovery) {
    deferred := receiver.Recover
    defer deferred()
    panic("unknown method target")
}
func nilInterfaceMethodBinding() {
    var receiver Recovery
    deferred := receiver.Recover
    defer deferred()
    panic("unreachable")
}
func nilValueReceiverMethodBinding() {
    var receiver *valueRecoverer
    deferred := receiver.Recover
    defer deferred()
    panic("unreachable")
}
func EffectAfterBoundValueRecovery(open Open) { recoveredByBoundValueMethod(); open() }
func EffectAfterBoundPointerRecovery(open Open) { recoveredByBoundPointerMethod(); open() }
func EffectAfterNilPointerRecovery(open Open) { recoveredByNilPointerMethod(); open() }
func EffectAfterBoundInterfaceRecovery(open Open) { recoveredByBoundInterfaceMethod(); open() }
func EffectAfterPromotedRecovery(open Open) { recoveredByPromotedMethod(); open() }
func EffectAfterPromotedPointerRecovery(open Open) { recoveredByPromotedPointerMethod(); open() }
func EffectAfterGenericBoundRecovery(open Open) { recoveredByGenericBoundMethod(); open() }
func EffectAfterGenericExpressionRecovery(open Open) { recoveredByGenericMethodExpression(); open() }
func EffectAfterConditionalBoundRecovery(open Open, enabled bool) { conditionalBoundMethodRecovery(enabled); open() }
func EffectAfterBoundRepanic(open Open) { boundMethodRecoveryRepanics(); open() }
func EffectAfterNormalBoundInvocation(open Open) { normalBoundMethodInvocationCannotRecover(); open() }
func EffectAfterNestedMethodRecovery(open Open) { nestedMethodRecoveryCannotRecover(); open() }
func EffectAfterAmbiguousBoundRecovery(open Open, enabled bool) { ambiguousBoundMethodRecovery(enabled); open() }
func EffectAfterUnknownBoundRecovery(open Open, receiver Recovery) { unknownBoundMethodRecovery(receiver); open() }
func EffectAfterNilInterfaceBinding(open Open) { nilInterfaceMethodBinding(); open() }
func EffectAfterNilValueReceiverBinding(open Open) { nilValueReceiverMethodBinding(); open() }
func EffectBeforeBoundRepanic(open Open) { open(); boundMethodRecoveryRepanics() }`,
		"owner/owner_test.go": `package owner
func unsafeAfterBoundValueRecovery() { EffectAfterBoundValueRecovery(Socket) }
func unsafeAfterBoundPointerRecovery() { EffectAfterBoundPointerRecovery(Socket) }
func unsafeAfterNilPointerRecovery() { EffectAfterNilPointerRecovery(Socket) }
func unsafeAfterBoundInterfaceRecovery() { EffectAfterBoundInterfaceRecovery(Socket) }
func unsafeAfterPromotedRecovery() { EffectAfterPromotedRecovery(Socket) }
func unsafeAfterPromotedPointerRecovery() { EffectAfterPromotedPointerRecovery(Socket) }
func unsafeAfterGenericBoundRecovery() { EffectAfterGenericBoundRecovery(Socket) }
func unsafeAfterGenericExpressionRecovery() { EffectAfterGenericExpressionRecovery(Socket) }
func unsafeEffectBeforeBoundRepanic() { EffectBeforeBoundRepanic(Socket) }
func safeAfterConditionalBoundRecovery(enabled bool) { EffectAfterConditionalBoundRecovery(Socket, enabled) }
func safeAfterBoundRepanic() { EffectAfterBoundRepanic(Socket) }
func safeAfterNormalBoundInvocation() { EffectAfterNormalBoundInvocation(Socket) }
func safeAfterNestedMethodRecovery() { EffectAfterNestedMethodRecovery(Socket) }
func safeAfterAmbiguousBoundRecovery(enabled bool) { EffectAfterAmbiguousBoundRecovery(Socket, enabled) }
func safeAfterUnknownBoundRecovery(receiver Recovery) { EffectAfterUnknownBoundRecovery(Socket, receiver) }
func safeAfterNilInterfaceBinding() { EffectAfterNilInterfaceBinding(Socket) }
func safeAfterNilValueReceiverBinding() { EffectAfterNilValueReceiverBinding(Socket) }`,
	})
	result, err := analyzeFixtureRoot(root)
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	details := strings.Join(result.violations, "\n")
	for _, name := range []string{
		"unsafeAfterBoundValueRecovery",
		"unsafeAfterBoundPointerRecovery",
		"unsafeAfterNilPointerRecovery",
		"unsafeAfterBoundInterfaceRecovery",
		"unsafeAfterPromotedRecovery",
		"unsafeAfterPromotedPointerRecovery",
		"unsafeAfterGenericBoundRecovery",
		"unsafeAfterGenericExpressionRecovery",
		"unsafeEffectBeforeBoundRepanic",
	} {
		if !strings.Contains(details, name) {
			t.Errorf("violations = %v, want bound-method continuation entry %q", result.violations, name)
		}
	}
	for _, name := range []string{
		"safeAfterConditionalBoundRecovery",
		"safeAfterBoundRepanic",
		"safeAfterNormalBoundInvocation",
		"safeAfterNestedMethodRecovery",
		"safeAfterAmbiguousBoundRecovery",
		"safeAfterUnknownBoundRecovery",
		"safeAfterNilInterfaceBinding",
		"safeAfterNilValueReceiverBinding",
	} {
		if strings.Contains(details, name) {
			t.Errorf("violations = %v, non-continuing method case %q must fail closed", result.violations, name)
		}
	}
	if !result.classified["owner"] {
		t.Fatalf("classified packages = %v, want owner", result.classified)
	}
}

type runtimeRecoveryObservation struct {
	recovered any
}

type runtimeRecovery interface {
	Recover()
}

type runtimeValueRecoverer struct {
	observation *runtimeRecoveryObservation
}

func (receiver runtimeValueRecoverer) Recover() {
	receiver.observation.recovered = recover()
}

type runtimePointerRecoverer struct {
	observation *runtimeRecoveryObservation
}

func (receiver *runtimePointerRecoverer) Recover() {
	receiver.observation.recovered = recover()
}

type runtimeNilPointerRecoverer struct{}

func (*runtimeNilPointerRecoverer) Recover() {
	_ = recover()
}

type runtimeNoRecoverer struct{}

func (runtimeNoRecoverer) Recover() {}

type runtimeConditionalRecoverer struct {
	observation *runtimeRecoveryObservation
	enabled     bool
}

func (receiver runtimeConditionalRecoverer) Recover() {
	if receiver.enabled {
		receiver.observation.recovered = recover()
	}
}

type runtimeRepanicRecoverer struct {
	observation *runtimeRecoveryObservation
}

func (receiver runtimeRepanicRecoverer) Recover() {
	receiver.observation.recovered = recover()
	panic("replacement panic")
}

type runtimeNestedRecoverer struct {
	observation *runtimeRecoveryObservation
}

func (receiver runtimeNestedRecoverer) Recover() {
	runtimeNestedRecover(receiver.observation)
}

func runtimeNestedRecover(observation *runtimeRecoveryObservation) {
	observation.recovered = recover()
}

type runtimeEmbeddedRecoverer struct {
	runtimeValueRecoverer
}

type runtimeGenericRecoverer[T any] struct {
	observation *runtimeRecoveryObservation
}

func (receiver runtimeGenericRecoverer[T]) Recover() {
	receiver.observation.recovered = recover()
}

const runtimeOriginalPanic = "original panic"

type runtimeRecoveryScenario uint8

const (
	runtimeRecoverValue runtimeRecoveryScenario = iota + 1
	runtimeRecoverPointer
	runtimeRecoverInterface
	runtimeRecoverPromoted
	runtimeRecoverGenericExpression
	runtimeRecoverNilPointer
	runtimeRecoverConditional
	runtimeRecoverRepanic
	runtimeRecoverRepanicAfterEffect
	runtimeRecoverOrdinaryCall
	runtimeRecoverNestedHelper
	runtimeRecoverDynamic
	runtimeRecoverNilInterfaceBinding
	runtimeRecoverNilValueBinding
)

func runtimeRecoverByValue(observation *runtimeRecoveryObservation) {
	deferred := (runtimeValueRecoverer{observation: observation}).Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverByPointer(observation *runtimeRecoveryObservation) {
	deferred := (&runtimePointerRecoverer{observation: observation}).Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverByInterface(observation *runtimeRecoveryObservation) {
	var receiver runtimeRecovery = runtimeValueRecoverer{observation: observation}
	deferred := receiver.Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverByPromotion(observation *runtimeRecoveryObservation) {
	receiver := runtimeEmbeddedRecoverer{runtimeValueRecoverer{observation: observation}}
	deferred := receiver.Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverByGenericExpression(observation *runtimeRecoveryObservation) {
	deferred := runtimeGenericRecoverer[int].Recover
	defer deferred(runtimeGenericRecoverer[int]{observation: observation})
	panic(runtimeOriginalPanic)
}

func runtimeRecoverByNilPointer() {
	var receiver *runtimeNilPointerRecoverer
	deferred := receiver.Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverConditionally(observation *runtimeRecoveryObservation, enabled bool) {
	deferred := (runtimeConditionalRecoverer{observation: observation, enabled: enabled}).Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverAndRepanic(observation *runtimeRecoveryObservation) {
	deferred := (runtimeRepanicRecoverer{observation: observation}).Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeInvokeBoundMethodNormally(observation *runtimeRecoveryObservation) {
	deferred := (runtimeValueRecoverer{observation: observation}).Recover
	defer func() { deferred() }()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverThroughNestedHelper(observation *runtimeRecoveryObservation) {
	deferred := (runtimeNestedRecoverer{observation: observation}).Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeRecoverThroughDynamicMethod(
	observation *runtimeRecoveryObservation,
	recovering bool,
) {
	var receiver runtimeRecovery = runtimeNoRecoverer{}
	if recovering {
		receiver = runtimeValueRecoverer{observation: observation}
	}
	deferred := receiver.Recover
	defer deferred()
	panic(runtimeOriginalPanic)
}

func runtimeBindNilInterfaceMethod(nonNil bool) {
	var receiver runtimeRecovery
	if nonNil {
		receiver = runtimeNoRecoverer{}
	}
	deferred := receiver.Recover
	defer deferred()
	panic("unreachable")
}

func runtimeBindNilValueMethod() {
	var receiver *runtimeValueRecoverer
	deferred := receiver.Recover
	defer deferred()
	panic("unreachable")
}

func runRuntimeRecoveryScenario(
	scenario runtimeRecoveryScenario,
	observation *runtimeRecoveryObservation,
	option bool,
) (caught any, effectsBefore, effectsAfter int) {
	defer func() {
		caught = recover()
	}()
	switch scenario {
	case runtimeRecoverValue:
		runtimeRecoverByValue(observation)
	case runtimeRecoverPointer:
		runtimeRecoverByPointer(observation)
	case runtimeRecoverInterface:
		runtimeRecoverByInterface(observation)
	case runtimeRecoverPromoted:
		runtimeRecoverByPromotion(observation)
	case runtimeRecoverGenericExpression:
		runtimeRecoverByGenericExpression(observation)
	case runtimeRecoverNilPointer:
		runtimeRecoverByNilPointer()
	case runtimeRecoverConditional:
		runtimeRecoverConditionally(observation, option)
	case runtimeRecoverRepanic:
		runtimeRecoverAndRepanic(observation)
	case runtimeRecoverRepanicAfterEffect:
		effectsBefore++
		runtimeRecoverAndRepanic(observation)
	case runtimeRecoverOrdinaryCall:
		runtimeInvokeBoundMethodNormally(observation)
	case runtimeRecoverNestedHelper:
		runtimeRecoverThroughNestedHelper(observation)
	case runtimeRecoverDynamic:
		runtimeRecoverThroughDynamicMethod(observation, option)
	case runtimeRecoverNilInterfaceBinding:
		runtimeBindNilInterfaceMethod(option)
	case runtimeRecoverNilValueBinding:
		runtimeBindNilValueMethod()
	default:
		panic("unknown runtime recovery scenario")
	}
	effectsAfter++
	return nil, effectsBefore, effectsAfter
}

func TestBoundMethodRecoveryRuntimeOracle(t *testing.T) {
	t.Run("bound receiver forms recover and continue", func(t *testing.T) {
		cases := []struct {
			name     string
			scenario runtimeRecoveryScenario
		}{
			{name: "value receiver", scenario: runtimeRecoverValue},
			{name: "pointer receiver", scenario: runtimeRecoverPointer},
			{name: "interface receiver", scenario: runtimeRecoverInterface},
			{name: "promoted receiver", scenario: runtimeRecoverPromoted},
			{name: "generic method expression", scenario: runtimeRecoverGenericExpression},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				observation := &runtimeRecoveryObservation{}
				caught, before, after := runRuntimeRecoveryScenario(test.scenario, observation, false)
				if caught != nil || before != 0 || after != 1 || observation.recovered != runtimeOriginalPanic {
					t.Fatalf("caught = %v, before = %d, after = %d, recovered = %v", caught, before, after, observation.recovered)
				}
			})
		}
	})

	t.Run("nil pointer receiver remains callable", func(t *testing.T) {
		caught, before, after := runRuntimeRecoveryScenario(runtimeRecoverNilPointer, nil, false)
		if caught != nil || before != 0 || after != 1 {
			t.Fatalf("caught = %v, before = %d, after = %d", caught, before, after)
		}
	})

	t.Run("conditional recovery preserves both outcomes", func(t *testing.T) {
		for _, enabled := range []bool{true, false} {
			observation := &runtimeRecoveryObservation{}
			caught, before, after := runRuntimeRecoveryScenario(
				runtimeRecoverConditional,
				observation,
				enabled,
			)
			if enabled {
				if caught != nil || before != 0 || after != 1 || observation.recovered != runtimeOriginalPanic {
					t.Fatalf("enabled: caught = %v, before = %d, after = %d, recovered = %v", caught, before, after, observation.recovered)
				}
			} else if caught != runtimeOriginalPanic || before != 0 || after != 0 || observation.recovered != nil {
				t.Fatalf("disabled: caught = %v, before = %d, after = %d, recovered = %v", caught, before, after, observation.recovered)
			}
		}
	})

	t.Run("repanic preserves effects before but not after the helper", func(t *testing.T) {
		for _, scenario := range []runtimeRecoveryScenario{runtimeRecoverRepanic, runtimeRecoverRepanicAfterEffect} {
			observation := &runtimeRecoveryObservation{}
			caught, before, after := runRuntimeRecoveryScenario(scenario, observation, false)
			wantBefore := 0
			if scenario == runtimeRecoverRepanicAfterEffect {
				wantBefore = 1
			}
			if caught != "replacement panic" || before != wantBefore || after != 0 || observation.recovered != runtimeOriginalPanic {
				t.Fatalf("scenario = %d: caught = %v, before = %d, after = %d, recovered = %v", scenario, caught, before, after, observation.recovered)
			}
		}
	})

	t.Run("ordinary and nested method calls cannot recover", func(t *testing.T) {
		for _, scenario := range []runtimeRecoveryScenario{runtimeRecoverOrdinaryCall, runtimeRecoverNestedHelper} {
			observation := &runtimeRecoveryObservation{}
			caught, before, after := runRuntimeRecoveryScenario(scenario, observation, false)
			if caught != runtimeOriginalPanic || before != 0 || after != 0 || observation.recovered != nil {
				t.Fatalf("scenario = %d: caught = %v, before = %d, after = %d, recovered = %v", scenario, caught, before, after, observation.recovered)
			}
		}
	})

	t.Run("dynamic method set follows the bound receiver", func(t *testing.T) {
		for _, recovering := range []bool{true, false} {
			observation := &runtimeRecoveryObservation{}
			caught, before, after := runRuntimeRecoveryScenario(runtimeRecoverDynamic, observation, recovering)
			if recovering {
				if caught != nil || before != 0 || after != 1 || observation.recovered != runtimeOriginalPanic {
					t.Fatalf("recovering: caught = %v, before = %d, after = %d, recovered = %v", caught, before, after, observation.recovered)
				}
			} else if caught != runtimeOriginalPanic || before != 0 || after != 0 {
				t.Fatalf("non-recovering: caught = %v, before = %d, after = %d", caught, before, after)
			}
		}
	})

	t.Run("nil method binding panics before registration", func(t *testing.T) {
		cases := []struct {
			name     string
			scenario runtimeRecoveryScenario
		}{
			{name: "nil interface", scenario: runtimeRecoverNilInterfaceBinding},
			{name: "nil pointer to value receiver", scenario: runtimeRecoverNilValueBinding},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				caught, before, after := runRuntimeRecoveryScenario(test.scenario, nil, false)
				if caught == nil || before != 0 || after != 0 {
					t.Fatalf("caught = %v, before = %d, after = %d", caught, before, after)
				}
			})
		}
	})
}

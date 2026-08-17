package outputcap

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestFileAccessMethodSetsArePurposeBound(t *testing.T) {
	tests := []struct {
		name    string
		access  reflect.Type
		methods []string
	}{
		{name: "identity", access: reflect.TypeFor[FileIdentity](), methods: []string{"Close", "SameFile", "Size"}},
		{name: "observed", access: reflect.TypeFor[ObservedFile](), methods: []string{"Close", "MetadataMatches", "ReadAt", "SameFile", "Size"}},
		{name: "recovery durability", access: reflect.TypeFor[RecoveryDurabilityFile](), methods: []string{"Close", "SameFile", "Size", "Sync"}},
		{name: "mutable", access: reflect.TypeFor[MutableFile](), methods: []string{"Close", "MetadataMatches", "ReadAt", "SameFile", "SetModifiedTime", "Size", "Sync", "WriteAt"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := make([]string, 0, test.access.NumMethod())
			for index := range test.access.NumMethod() {
				got = append(got, test.access.Method(index).Name)
			}
			slices.Sort(got)
			slices.Sort(test.methods)
			if !slices.Equal(got, test.methods) {
				t.Fatalf("method set = %v, want %v", got, test.methods)
			}
		})
	}

	if _, found := reflect.TypeFor[Directory]().MethodByName("OpenFile"); found {
		t.Fatal("directory retained the Boolean file access API")
	}
}

func TestNativeErrorClassIsClosedAndCarrierIsTextFree(t *testing.T) {
	tests := []struct {
		class NativeErrorClass
		text  string
	}{
		{NativeErrorAccessDenied, "access_denied"},
		{NativeErrorSharingViolation, "sharing_violation"},
		{NativeErrorNotFound, "not_found"},
		{NativeErrorInvalidHandle, "invalid_handle"},
		{NativeErrorUnsupported, "unsupported"},
		{NativeErrorIO, "io"},
		{NativeErrorUnknown, "unknown"},
	}
	for _, test := range tests {
		if !test.class.Valid() || test.class.String() != test.text {
			t.Fatalf("class %d = valid %t text %q", test.class, test.class.Valid(), test.class.String())
		}
	}
	if NativeErrorClass(0).Valid() || NativeErrorClass(0).String() != "" ||
		NativeErrorClass(255).Valid() || NativeErrorClass(255).String() != "" {
		t.Fatal("unknown native error class escaped the closed vocabulary")
	}

	canary := errors.New("provider path and native detail canary")
	err := errors.Join(canary, nativeClassTestCarrier{class: NativeErrorAccessDenied})
	class, ok := ClassifyNativeError(err)
	if !ok || class != NativeErrorAccessDenied || class.String() == canary.Error() {
		t.Fatalf("classified error = %v, %t", class, ok)
	}
	if _, ok := ClassifyNativeError(canary); ok {
		t.Fatal("untyped provider error unexpectedly acquired a native class")
	}
}

type nativeClassTestCarrier struct {
	class NativeErrorClass
}

func (carrier nativeClassTestCarrier) Error() string { return "native-error" }
func (carrier nativeClassTestCarrier) NativeErrorClass() NativeErrorClass {
	return carrier.class
}

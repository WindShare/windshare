package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTransportRejectsConnectivityThroughAnyDependency(t *testing.T) {
	packages := []listedPackage{
		{ImportPath: transportPath + "/webrtc", Deps: []string{
			modulePath + "/core/framechannel", "github.com/pion/webrtc/v4",
		}},
		{ImportPath: transportPath + "/wrapped", Deps: []string{
			modulePath + "/bridge", connectivityPath + "/pionpeer",
		}},
		{ImportPath: transportPath + "/platform", Deps: []string{
			connectivityPath + "/socketauthority", modulePath + "/connectivityexamples",
		}},
	}
	var graph strings.Builder
	for _, pkg := range packages {
		if err := json.NewEncoder(&graph).Encode(pkg); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findings(strings.NewReader(graph.String()))
	want := []string{
		transportPath + "/platform depends on " + connectivityPath + "/socketauthority",
		transportPath + "/wrapped depends on " + connectivityPath + "/pionpeer",
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, %v; want %v", got, err, want)
	}
}

func TestMalformedGraphCannotPassBoundary(t *testing.T) {
	if _, err := findings(strings.NewReader("{")); err == nil {
		t.Fatal("malformed dependency graph accepted")
	}
}

func TestTargetEnvironmentOverridesAmbientPlatformAndWorkspace(t *testing.T) {
	got := targetEnvironment([]string{
		"Path=tools", "GOOS=plan9", "GOARCH=arm", "gowork=external", "GOTOOLCHAIN=auto",
	}, "linux")
	want := []string{"Path=tools", "GOOS=linux", "GOARCH=amd64", "GOWORK=off", "GOTOOLCHAIN=local"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("target environment = %v; want %v", got, want)
	}
}

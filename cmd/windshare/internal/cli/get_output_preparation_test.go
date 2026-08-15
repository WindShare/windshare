package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type bindingOnlyGetOutputAuthority struct {
	getOutputAuthority
	events *[]string
}

func (authority *bindingOnlyGetOutputAuthority) BindDestination(context.Context) (getOutputMode, error) {
	*authority.events = append(*authority.events, "bind")
	return getOutputResumable, nil
}

func (authority *bindingOnlyGetOutputAuthority) Close() error {
	*authority.events = append(*authority.events, "close")
	return nil
}

func TestGetDefaultsToCurrentDirectoryAndBindsItBeforeSessionWork(t *testing.T) {
	capability := newSemanticCapability(t, "wss://relay.example")
	encoded, err := capability.URL("https://app.example")
	if err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := newSemanticTestApp(strings.NewReader(""))
	request, code := app.parseGetRequest([]string{encoded})
	if code != ExitOK || request.outDir != "." {
		t.Fatalf("request=%+v exit=%d stderr=%q", request, code, stderr.String())
	}

	var (
		config getOutputAuthorityConfig
		events []string
	)
	app.getOutputFactory = getOutputAuthorityFactoryFunc(func(candidate getOutputAuthorityConfig) (getOutputAuthority, error) {
		config = candidate
		events = append(events, "construct")
		return &bindingOnlyGetOutputAuthority{events: &events}, nil
	})
	prepared, code := app.prepareGetOutput(context.Background(), request)
	if code != ExitOK {
		t.Fatalf("prepare exit=%d stderr=%q", code, stderr.String())
	}
	wantRoot, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if config.rootPath != wantRoot || !config.createRoot || config.tracer == nil {
		t.Fatalf("output config=%+v want_root=%q", config, wantRoot)
	}
	if strings.Join(events, ",") != "construct,bind" {
		t.Fatalf("pre-session events=%v", events)
	}
	if err := prepared.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "construct,bind,close" {
		t.Fatalf("authority lifecycle=%v", events)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("preparation wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

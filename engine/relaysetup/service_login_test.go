package relaysetup

import "testing"

func TestServiceLoginUsesOnlyTheDiscoveredDestination(t *testing.T) {
	var pending PendingSetup
	const invite = "aW52aXRl"
	if login, err := pending.serviceLogin(invite); err != nil || login != nil {
		t.Fatal(login, err)
	}
	pending.AccountLoginPage = "https://accounts.example:8443/device"
	login, err := pending.serviceLogin(invite)
	if err != nil || login == nil || login.Host != "accounts.example:8443" || login.URL != "https://accounts.example:8443/device#invite="+invite {
		t.Fatal(login, err)
	}
	for _, invalid := range []string{"https://other.example/login", invite + "\n"} {
		if _, err = pending.serviceLogin(invalid); err == nil {
			t.Fatalf("accepted invalid invitation %q", invalid)
		}
	}
	pending.AccountLoginPage = "http://accounts.example/device"
	if _, err = pending.serviceLogin(invite); err == nil {
		t.Fatal("accepted insecure login page")
	}
}

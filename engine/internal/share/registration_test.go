package share

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestRegistrationMaterialUsesEd25519Width(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateKey) != ed25519.PrivateKeySize || catalog.IdentityBytes != v2.ShareInstanceBytes {
		t.Fatal("suite-02 identity widths diverged")
	}
}

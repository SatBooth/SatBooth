package service

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/satbooth/satbooth/nostr"
)

const (
	testAPIKey  = "write-key-for-tests-0123456789abcdef"
	testReadKey = "read-key-for-tests-0123456789abcdef"
	testWebhook = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	testAPIKey2 = "rotated-write-key-0123456789abcdef"
)

func keysJSON(entries ...[3]string) []byte {
	s := `{"keys":[`
	for i, e := range entries {
		if i > 0 {
			s += ","
		}
		h := sha256.Sum256([]byte(e[2]))
		s += `{"id":"` + e[0] + `","capability":"` + e[1] + `","sha256":"` + hex.EncodeToString(h[:]) + `"}`
	}
	return []byte(s + `]}`)
}

// writeFixtures writes a client key, an API key file and a webhook secret, all 0600, and returns their paths.
func writeFixtures(t testing.TB, dir string) (keyPath, apiPath, whPath string) {
	t.Helper()
	k, err := nostr.GenerateSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "client.key")
	if err := nostr.WriteNewSecretKeyFile(keyPath, k); err != nil {
		t.Fatal(err)
	}
	k.Zero()
	apiPath = filepath.Join(dir, "api-keys.json")
	if err := os.WriteFile(apiPath, keysJSON([3]string{"merchant", "orders:write", testAPIKey}, [3]string{"reader", "orders:read", testReadKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	whPath = filepath.Join(dir, "webhook.secret")
	if err := os.WriteFile(whPath, []byte(testWebhook+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

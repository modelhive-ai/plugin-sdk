package plugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// resourceTokenLabel separates the resource token from the request
// signatures made with the same secret.
const resourceTokenLabel = "sundelion/plugin-resources/v1"

// ResourceToken is the bearer token an out-of-process instance presents on
// the gateway's resource callback (/api/v1/plugin-resources/). Both sides
// derive it from the instance's HMAC secret, so it needs no delivery of its
// own and changes when the secret is rotated. Knowing the token does not
// reveal the secret or allow signing calls to the plugin.
func ResourceToken(hmacSecret string) string {
	if hmacSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(hmacSecret))
	mac.Write([]byte(resourceTokenLabel))
	return hex.EncodeToString(mac.Sum(nil))
}

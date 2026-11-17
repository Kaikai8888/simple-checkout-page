// Package signature signs gateway <-> merchant messages with HMAC-SHA256.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

const Header = "X-Signature"

func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret string, body []byte, sig string) bool {
	expected, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}

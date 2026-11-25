package redact

import (
	"encoding/base64"
	"net/netip"
	"strings"
)

func decodesToUserPass(tok string) bool {
	tok = strings.TrimRight(tok, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(tok); err == nil {
			return strings.Contains(string(b), ":")
		}
	}
	return false
}

// checkIP accepts a candidate only when it parses as a real address, so
// version strings, times and similar digit/colon runs are kept.
func checkIP(m string) bool {
	a, err := netip.ParseAddr(m)
	return err == nil && a.IsValid()
}

package chrome

import (
	"strings"
	"testing"
)

func TestExtractManual(t *testing.T) {
	if testing.Short() {
		t.Skip("manual extraction test")
	}
	cookies, err := ExtractCookies("chatgpt.com")
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	t.Logf("chatgpt cookies: %d", len(cookies))
	for _, c := range cookies {
		v := c.Value
		if len(v) > 24 {
			v = v[:24] + "..."
		}
		t.Logf("  %s (domain=%s, len=%d) %s", c.Name, c.Domain, len(c.Value), v)
	}
	hdr := CookieHeader(cookies)
	t.Logf("header length: %d", len(hdr))
	if !strings.Contains(hdr, "__Secure-next-auth.session-token.0=") {
		t.Fatalf("session token fragment missing")
	}
}

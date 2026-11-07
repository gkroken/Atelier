package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackOnly(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.2:0"} {
		if err := loopbackOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	// Without sign-in, any of these would let other machines act as the account.
	for _, bad := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.5:8080", "example.com:8080", "8080"} {
		if err := loopbackOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// State-changing requests from another site (or with no sign of where they come from)
// are refused, so a web page can't start or quit sessions through the user's browser.
func TestSameOriginRefusesCrossSiteWrites(t *testing.T) {
	ran := false
	h := sameOrigin(func(w http.ResponseWriter, r *http.Request) { ran = true })
	for name, c := range map[string]struct {
		headers map[string]string
		allow   bool
	}{
		"same-origin fetch metadata": {map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		"matching Origin":            {map[string]string{"Origin": "http://127.0.0.1:8080"}, true},
		"cross-site":                 {map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://127.0.0.1:8080"}, false},
		"same-site":                  {map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		"none (typed URL)":           {map[string]string{"Sec-Fetch-Site": "none"}, false},
		"other Origin":               {map[string]string{"Origin": "https://evil.example"}, false},
		"other port":                 {map[string]string{"Origin": "http://127.0.0.1:9999"}, false},
		"null Origin":                {map[string]string{"Origin": "null"}, false},
		"no headers":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			ran = false
			r := httptest.NewRequest("POST", "http://127.0.0.1:8080/start", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h(w, r)
			if ran != c.allow {
				t.Errorf("handler ran = %v, want %v (status %d)", ran, c.allow, w.Code)
			}
			if !c.allow && w.Code != http.StatusForbidden {
				t.Errorf("status %d, want 403", w.Code)
			}
		})
	}
}

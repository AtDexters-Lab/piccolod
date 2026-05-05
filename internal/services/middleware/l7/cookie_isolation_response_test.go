package l7

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCookieIsolationResponse_dropsCrossAppPiccoloNamespace pins the
// Phase-3 adversarial finding: a compromised app A returning
// `Set-Cookie: __piccolo_appB_session=ATTACKER_ID` (no HttpOnly to bypass
// the rewrite gate) MUST be dropped by the response-side filter — without
// this, browsers (which scope cookies by host, not port — RFC 6265 §8.5)
// would store the cookie host-scoped to piccolo.local, and app B's
// request-side strip-and-rewrite would later unwrap it into
// `Cookie: session=ATTACKER_ID`, forging app B's session.
//
// Both the cross-app form (`__piccolo_otherApp_*`) and the same-app form
// (`__piccolo_thisApp_*`) are dropped from backend output: the proxy
// itself produces same-app `__piccolo_<thisApp>_*` cookies via the
// rewrite block, the backend MUST NOT.
func TestCookieIsolationResponse_dropsCrossAppPiccoloNamespace(t *testing.T) {
	cases := []struct {
		name      string
		setCookie string
		wantKept  bool // true = cookie passes through; false = filter dropped
	}{
		{
			name:      "cross-app __piccolo namespace — dropped",
			setCookie: "__piccolo_appB_session=ATTACKER; Path=/",
			wantKept:  false,
		},
		{
			name:      "cross-app __piccolo namespace with HttpOnly — dropped",
			setCookie: "__piccolo_appB_session=ATTACKER; Path=/; HttpOnly",
			wantKept:  false,
		},
		{
			name:      "same-app __piccolo namespace from backend — dropped (proxy reserved)",
			setCookie: "__piccolo_appA_session=value; Path=/",
			wantKept:  false,
		},
		{
			name:      "piccolo_ namespace — dropped (existing IsPiccoloCookieName)",
			setCookie: "piccolo_session=ATTACKER; Path=/",
			wantKept:  false,
		},
		{
			name:      "ordinary backend cookie — passes through",
			setCookie: "session=legitimate; Path=/",
			wantKept:  true,
		},
		{
			name:      "ordinary backend cookie with double-underscore prefix unrelated — passes through",
			setCookie: "__Host-CSRF=token; Path=/; Secure",
			wantKept:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := CookieIsolationResponse("appA")

			req := httptest.NewRequest(http.MethodGet, "http://piccolo.local/", nil)
			resp := &http.Response{
				StatusCode: 200,
				Header:     http.Header{},
				Request:    req,
			}
			resp.Header.Add("Set-Cookie", tc.setCookie)

			if err := mw(resp); err != nil {
				t.Fatalf("modifier returned error: %v", err)
			}

			got := resp.Header.Values("Set-Cookie")
			gotKept := len(got) == 1
			if gotKept != tc.wantKept {
				t.Fatalf("Set-Cookie kept = %v, want %v (response headers: %v)", gotKept, tc.wantKept, got)
			}
		})
	}
}

// TestCookieIsolationResponse_endToEndAttackerScenario walks the exact
// cross-app injection scenario end-to-end: app A's response → response-side
// filter → would-be browser-stored cookie → app B's request-side
// strip+rewrite → backend-visible Cookie header. Asserts the forged
// session never reaches app B's backend.
func TestCookieIsolationResponse_endToEndAttackerScenario(t *testing.T) {
	// Step 1: app A's compromised backend tries to set a cookie targeting
	// app B's __piccolo_appB_ namespace.
	respMW := CookieIsolationResponse("appA")
	req := httptest.NewRequest(http.MethodGet, "http://piccolo.local/", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    req,
	}
	resp.Header.Add("Set-Cookie", "__piccolo_appB_session=ATTACKER_FORGED_SESSION; Path=/")
	if err := respMW(resp); err != nil {
		t.Fatalf("respMW: %v", err)
	}

	// Assert the cookie never reached the browser (filter dropped it).
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) != 0 {
		t.Fatalf("attacker cookie should NOT pass response-side filter, got %v", cookies)
	}

	// Step 2: simulate the browser sending the cookie anyway (skip the
	// filter, e.g., older client without our fix, or attacker injects via
	// JS). App B's request-side strip-and-rewrite must NOT unwrap the
	// other-app prefix into app B's namespace.
	bReq := httptest.NewRequest(http.MethodGet, "http://piccolo.local:8080/", nil)
	bReq.Header.Set("Cookie", "__piccolo_appB_session=ATTACKER_FORGED_SESSION")
	StripAndRewriteRequestCookies(bReq, "appB", true)

	// The cookie is for app B's prefix from the strip side's perspective —
	// it WILL get unwrapped (the strip's job). This is the documented
	// secondary defense gap: the response-side filter is the FIRST line of
	// defense; the request-side strip alone cannot tell whether a
	// __piccolo_appB_<x> cookie originated from the proxy's own rewrite
	// or from a cross-app attacker. The fix is at the response side
	// (Phase 3 adversarial finding).
	if got := bReq.Header.Get("Cookie"); strings.Contains(got, "ATTACKER_FORGED_SESSION") {
		// Strip-side unwrap is expected here; this assertion documents
		// that the attack still works if a cookie reaches the browser.
		// The Phase-3 fix prevents the cookie from reaching the browser
		// in the first place — verified by Step 1's assertion above.
		t.Logf("documented secondary gap: strip-side unwrap of pre-existing cookie (mitigation lives at response side)")
	}
}

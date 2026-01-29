package remote

import (
	"errors"
	"testing"
	"time"

	legoacme "github.com/go-acme/lego/v4/acme"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantClass     FailureClass
		wantCode      string
	}{
		{
			name:      "nil error",
			err:       nil,
			wantClass: FailureClassTransient,
			wantCode:  "cert_unknown_error",
		},
		{
			name:      "generic error",
			err:       errors.New("something went wrong"),
			wantClass: FailureClassTransient,
			wantCode:  "cert_unknown_error",
		},
		{
			name: "rate limited problem details",
			err: &legoacme.ProblemDetails{
				Type:       "urn:ietf:params:acme:error:rateLimited",
				HTTPStatus: 429,
			},
			wantClass: FailureClassRateLimited,
			wantCode:  "cert_rate_limited",
		},
		{
			name: "DNS error",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:dns",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_dns_error",
		},
		{
			name: "unauthorized with NXDOMAIN detail",
			err: &legoacme.ProblemDetails{
				Type:   "urn:ietf:params:acme:error:unauthorized",
				Detail: "DNS problem: NXDOMAIN looking up A for example.com",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_domain_unreachable",
		},
		{
			name: "unauthorized with no valid IPs detail",
			err: &legoacme.ProblemDetails{
				Type:   "urn:ietf:params:acme:error:unauthorized",
				Detail: "No valid IP addresses found for example.com",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_domain_unreachable",
		},
		{
			name: "unauthorized generic",
			err: &legoacme.ProblemDetails{
				Type:   "urn:ietf:params:acme:error:unauthorized",
				Detail: "Challenge verification failed",
			},
			wantClass: FailureClassTransient,
			wantCode:  "cert_unauthorized",
		},
		{
			name: "connection error",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:connection",
			},
			wantClass: FailureClassTransient,
			wantCode:  "cert_connection_failed",
		},
		{
			name: "CAA forbidden",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:caa",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_caa_forbidden",
		},
		{
			name: "rejected identifier",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:rejectedIdentifier",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_rejected_identifier",
		},
		{
			name: "invalid contact",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:invalidContact",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_invalid_contact",
		},
		{
			name: "account does not exist",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:accountDoesNotExist",
			},
			wantClass: FailureClassConfigError,
			wantCode:  "cert_account_error",
		},
		{
			name: "unknown ACME error",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:unknownError",
			},
			wantClass: FailureClassTransient,
			wantCode:  "cert_acme_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotCode := classifyFailure(tt.err)
			if gotClass != tt.wantClass {
				t.Errorf("classifyFailure() class = %v, want %v", gotClass, tt.wantClass)
			}
			if gotCode != tt.wantCode {
				t.Errorf("classifyFailure() code = %v, want %v", gotCode, tt.wantCode)
			}
		})
	}
}

func TestIsRateLimitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "RateLimitedError type",
			err:  &legoacme.RateLimitedError{},
			want: true,
		},
		{
			name: "ProblemDetails with rate limit type",
			err: &legoacme.ProblemDetails{
				Type: "urn:ietf:params:acme:error:rateLimited",
			},
			want: true,
		},
		{
			name: "ProblemDetails with 429 status",
			err: &legoacme.ProblemDetails{
				HTTPStatus: 429,
			},
			want: true,
		},
		{
			name: "error message contains rate limit",
			err:  errors.New("too many requests - rate limit exceeded"),
			want: true,
		},
		{
			name: "error message contains too many certificates",
			err:  errors.New("Error: too many certificates already issued"),
			want: true,
		},
		{
			name: "error message contains too many failed authorizations",
			err:  errors.New("too many failed authorizations recently"),
			want: true,
		},
		{
			name: "error with unrelated 429 string",
			err:  errors.New("port 42900 is blocked"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRateLimitError(tt.err); got != tt.want {
				t.Errorf("isRateLimitError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantNil  bool
		wantFunc func(*time.Time) bool // custom validation
	}{
		{
			name:    "nil error",
			err:     nil,
			wantNil: true,
		},
		{
			name:    "non-RateLimitedError",
			err:     errors.New("generic error"),
			wantNil: true,
		},
		{
			name: "RateLimitedError with no RetryAfter",
			err:  &legoacme.RateLimitedError{},
			wantNil: true,
		},
		{
			name: "RateLimitedError with seconds",
			err: &legoacme.RateLimitedError{
				RetryAfter: "3600",
			},
			wantNil: false,
			wantFunc: func(t *time.Time) bool {
				if t == nil {
					return false
				}
				// Should be approximately 1 hour from now
				diff := time.Until(*t)
				return diff > 55*time.Minute && diff < 65*time.Minute
			},
		},
		{
			name: "RateLimitedError with HTTP-date (IMF-fixdate)",
			err: &legoacme.RateLimitedError{
				// IMF-fixdate format as per HTTP spec: "Sun, 06 Nov 1994 08:49:37 GMT"
				RetryAfter: time.Now().Add(2 * time.Hour).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"),
			},
			wantNil: false,
			wantFunc: func(t *time.Time) bool {
				if t == nil {
					return false
				}
				// Should be approximately 2 hours from now
				diff := time.Until(*t)
				return diff > 115*time.Minute && diff < 125*time.Minute
			},
		},
		{
			name: "RateLimitedError with invalid RetryAfter",
			err: &legoacme.RateLimitedError{
				RetryAfter: "invalid",
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRetryAfter(tt.err)
			if tt.wantNil && got != nil {
				t.Errorf("parseRetryAfter() = %v, want nil", got)
				return
			}
			if !tt.wantNil && got == nil {
				t.Errorf("parseRetryAfter() = nil, want non-nil")
				return
			}
			if tt.wantFunc != nil && !tt.wantFunc(got) {
				t.Errorf("parseRetryAfter() = %v, failed validation", got)
			}
		})
	}
}

func TestReasonForCode(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"ok", "Operational"},
		{"cert_dns_error", "Domain DNS not configured"},
		{"cert_rate_limited", "Rate limited by Let's Encrypt"},
		{"cert_pending", "Certificate issuance in progress"},
		{"backend_unreachable", "Backend not responding"},
		{"unknown_code", "Unknown status"},
		{"", "Unknown status"},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := ReasonForCode(tt.code); got != tt.want {
				t.Errorf("ReasonForCode(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

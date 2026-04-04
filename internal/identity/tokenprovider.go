package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	backend "github.com/AtDexters-Lab/nexus-proxy/client"
	"github.com/AtDexters-Lab/namek-server/pkg/namekclient"
)

// NamekTokenProvider bridges namekclient.RequestNexusToken to backend.TokenProvider.
type NamekTokenProvider struct {
	clientFn func() *namekclient.Client
	onError  func(err error, httpStatus int)
}

// NewNamekTokenProvider creates a token provider that uses the namekclient for TPM-attested tokens.
// clientFn is a getter (not a direct pointer) so the provider always uses the current client
// even after re-enrollment creates a new instance.
func NewNamekTokenProvider(clientFn func() *namekclient.Client, onError func(err error, httpStatus int)) *NamekTokenProvider {
	return &NamekTokenProvider{clientFn: clientFn, onError: onError}
}

func (p *NamekTokenProvider) IssueToken(ctx context.Context, req backend.TokenRequest) (backend.Token, error) {
	nc := p.clientFn()
	if nc == nil {
		return backend.Token{}, fmt.Errorf("namek client not available")
	}
	stage := tokenStageToInt(req.Stage)
	token, err := nc.RequestNexusToken(ctx, stage, req.SessionNonce)
	if err != nil {
		if p.onError != nil {
			if status := extractHTTPStatus(err); status > 0 {
				p.onError(err, status)
			}
		}
		return backend.Token{}, err
	}
	return backend.Token{Value: token}, nil
}

// tokenStageToInt maps backend.TokenStage to the integer stages expected by namekclient.
func tokenStageToInt(stage backend.TokenStage) int {
	switch stage {
	case backend.StageHandshake:
		return 0
	case backend.StageAttest:
		return 1
	case backend.StageReauth:
		return 2
	default:
		return 0
	}
}

// extractHTTPStatus attempts to extract an HTTP status code from an error.
// namekclient returns errors like "server error 401: ...".
func extractHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	var status int
	if strings.HasPrefix(msg, "server error ") {
		_, _ = fmt.Sscanf(msg, "server error %d:", &status)
	}
	return status
}

// extractErrorMessage extracts the "error" field from a namekclient error body.
// namekclient errors are formatted as "server error NNN: <body>" where <body> is
// typically JSON like {"error":"device not found"}.
// Returns the parsed error string, the raw body, or empty string if unparseable.
func extractErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "server error ") {
		return ""
	}
	idx := strings.Index(msg, ": ")
	if idx < 0 {
		return ""
	}
	body := msg[idx+2:]
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	return body
}

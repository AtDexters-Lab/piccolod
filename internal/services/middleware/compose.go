package middleware

import (
	"net"
	"net/http"
)

// ComposeRequestChain wraps the terminal handler with each L7Middleware in
// order: chain[0] is the outermost wrapper, chain[len-1] is the innermost
// (closest to terminal). Empty chain returns terminal unchanged.
//
// Equivalent to:
//
//	chain[0](chain[1](...chain[n-1](terminal)...))
func ComposeRequestChain(chain []L7Middleware, terminal http.Handler) http.Handler {
	h := terminal
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	return h
}

// ComposeResponseChain returns a single ResponseModifier that runs each modifier
// in order. First non-nil error short-circuits — remaining modifiers are not
// invoked (matches httputil.ReverseProxy.ModifyResponse semantics: an error
// here causes the proxy to emit 502).
func ComposeResponseChain(chain []ResponseModifier) ResponseModifier {
	if len(chain) == 0 {
		return nil
	}
	if len(chain) == 1 {
		return chain[0]
	}
	return func(resp *http.Response) error {
		for _, mod := range chain {
			if err := mod(resp); err != nil {
				return err
			}
		}
		return nil
	}
}

// ComposeL4Chain wraps the terminal ConnHandler with each L4Middleware in
// order: chain[0] is the outermost wrapper, chain[len-1] is innermost (closest
// to terminal). Empty chain returns terminal unchanged.
func ComposeL4Chain(chain []L4Middleware, terminal ConnHandler) ConnHandler {
	h := terminal
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	return h
}

// ComposeL4UDPChain wraps the terminal UDPHandler with each L4UDPMiddleware
// in order. Same semantics as ComposeL4Chain, in the UDP shape.
func ComposeL4UDPChain(chain []L4UDPMiddleware, terminal UDPHandler) UDPHandler {
	h := terminal
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	return h
}

// DeriveSourceTrust classifies a connection's local address into its
// SourceTrust grade per plan §D14. A connection accepted on a loopback local
// address is TrustedLoopback (the proxy itself or the TLS mux relayed it);
// anything else is Direct.
//
// Falls back to Direct on nil or unparseable addresses (fail-closed —
// untrusted is the safer default for IP-rule middlewares).
func DeriveSourceTrust(localAddr net.Addr) SourceTrust {
	if localAddr == nil {
		return Direct
	}
	host, _, err := net.SplitHostPort(localAddr.String())
	if err != nil {
		host = localAddr.String()
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return TrustedLoopback
	}
	return Direct
}

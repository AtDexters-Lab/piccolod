package middleware

import "net/http"

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

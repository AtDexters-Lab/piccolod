// Package middleware implements the layered L4/L7 middleware framework for
// listener pipelines per RFC 20260505 (protocol-agnostic listener pipeline).
//
// Two parallel chains:
//
//   - L4 chain: operates on net.Conn (TCP) or per-datagram (UDP). Applies to
//     every listener regardless of (flow, protocol). Sees source address,
//     local address, listener identity, byte counts. Cannot inspect HTTP
//     semantics. Built-ins: ip_allowlist, ip_rate_limit, conn_metrics,
//     connection_auth, hint_consumer_l4.
//
//   - L7 chain: operates on http.Handler. Applies only when piccolod parses
//     the application protocol (flow:tcp + protocol:http|websocket). Sees
//     full request, headers, path, method. Built-ins: forwarded_scrub,
//     hint_consumer_l7, path_normalize, reserved_path_intercept, path_auth,
//     proxy_oidc, oidc_authorize_snapshot, forward_headers, reverse_proxy.
//     Plus response-side ResponseModifier chain composed into a single
//     httputil.ReverseProxy.ModifyResponse: security_headers_response,
//     cookie_isolation_response, embedded_marker_response,
//     oidc_authorize_rewrite_response.
//
// Package boundary: middleware does NOT import internal/services to avoid an
// import cycle (services imports middleware to use the registry). Endpoint
// metadata reaches middleware as an EndpointInfo struct constructed by the
// services package from a ServiceEndpoint. internal/api is a leaf package and
// is imported here for the typed Flow/Protocol enums.
//
// Plan: .claude/plans/protocol-agnostic-listener-pipeline.md
package middleware

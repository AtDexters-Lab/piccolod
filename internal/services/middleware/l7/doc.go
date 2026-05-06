// Package l7 contains the named L7 middlewares that decompose today's inline
// startHTTPProxy handler in internal/services/proxy.go.
//
// Each file exports a factory function that returns a middleware.L7Middleware
// (request-side) or middleware.ResponseModifier (response-side). Factories take
// their dependencies as explicit parameters; step 2 of the layered-pipeline plan
// (.claude/plans/protocol-agnostic-listener-pipeline.md §H) is just the extraction
// — startHTTPProxy continues to call them inline in the existing canonical order.
// Step 3 will replace the inline composition with registry.Build.
//
// The canonical order matches today's startHTTPProxy body bit-for-bit. Reordering
// or behavior changes are out-of-scope for step 2.
package l7

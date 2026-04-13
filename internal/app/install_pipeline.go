package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"piccolod/internal/api"
	"piccolod/internal/hostname"

	"gopkg.in/yaml.v3"
)

// authSecretScanSentinel is a unique value substituted for
// .System.Auth.ClientSecret during the S1' invariant scan. The scanner then
// walks the rendered YAML looking for the sentinel to determine which YAML
// fields end up containing the secret. Picked to be impossible to match in
// normal manifests (long, distinctive prefix, no shell-special characters).
const authSecretScanSentinel = "__PICCOLO_S1_SCAN_SENTINEL_d4f7a1c8e6b9__"

// Sha256Hex returns the lowercase hex-encoded SHA-256 digest of b. Shared
// across the catalog sync subsystem so install-time and sync-time hashes
// cannot drift.
func Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InstallSystemContext is the frozen subset of .System.* fields captured at install
// and replayed on sync. Freezing the context guarantees sync re-renders are
// deterministic regardless of runtime machine state drift (portal changes,
// timezone changes, hostname changes).
//
// Auth is intentionally NOT part of this struct because the OIDC issuer is
// derived from machine-stable mDNS state (not drift-prone) and the OIDC
// credentials live in InstallState.OIDCCredentials.
type InstallSystemContext struct {
	Domain       string `json:"domain"`
	Architecture string `json:"architecture"`
	Timezone     string `json:"timezone"`
	IssuerHint   string `json:"issuer_hint"`
}

// OIDCCredentials carries the plaintext OIDC credentials for an app. The
// plaintext form is required for sync to re-render templates that interpolate
// {{ .System.Auth.ClientID/ClientSecret }} into service env vars; the OIDC
// client manager only stores an argon2id hash, so plaintext must be
// independently persisted (in InstallState).
type OIDCCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// AppLister is the minimal interface needed by the install pipeline to check
// for primary-listener-name collisions during install. At install time the
// caller passes nil for skipInstanceID; on sync re-render the caller passes
// the current app's instance ID so the app does not collide with itself.
type AppLister interface {
	List(ctx context.Context) ([]*AppInstance, error)
}

// OIDCClientGenerator is the subset of oidc.ClientManager required by the
// pipeline. Sync paths that already have credentials may pass nil because the
// pipeline never calls Generate when ExistingOIDC is set.
type OIDCClientGenerator interface {
	GenerateCredentials() (clientID, clientSecret string, err error)
}

// InstallPipelineInput is the full set of inputs the pipeline needs to render
// an app definition deterministically.
type InstallPipelineInput struct {
	// RawTemplate is the raw catalog manifest bytes (pre-render).
	RawTemplate []byte
	// UserInputs is the user-provided input map. For sync, this is the persisted
	// install_state.json InstallInputs map (verbatim).
	UserInputs map[string]interface{}
	// SystemContext is the frozen .System.* snapshot. On install this is built
	// from live host state via SyncHost.CurrentInstallSystemContext(); on sync
	// it is loaded from install_state.json verbatim.
	SystemContext InstallSystemContext
	// InstanceID is required by sync to skip self in primary-name collision
	// checks. Empty on install (the pipeline does not derive instance IDs;
	// install handler does that after substitution from __app_address__).
	InstanceID string
	// ExistingOIDC, if non-nil, instructs the pipeline to use these credentials
	// verbatim instead of generating fresh ones. Set on sync re-render.
	ExistingOIDC *OIDCCredentials
}

// InstallPipelineResult is the post-render output of the pipeline.
type InstallPipelineResult struct {
	// Definition is the parsed and validated app definition with __primary
	// substitution applied (if applicable).
	Definition *api.AppDefinition
	// OIDCCredentials are the credentials in use by the rendered manifest:
	// freshly generated on install, passed-through from ExistingOIDC on sync.
	// nil if the manifest does not declare any oidc_client.
	OIDCCredentials *OIDCCredentials
	// CanonicalBytes is the post-SetDefaults yaml.Marshal of Definition. Used
	// for byte-equal diff comparisons (sync). Distinct from RawTemplate.
	CanonicalBytes []byte
	// RawTemplateHash is the sha256 of RawTemplate. Used as the catalog drift
	// detection hash.
	RawTemplateHash string
	// UsedSecretOnlyInInitScript is the S1' invariant. true means the
	// .System.Auth.ClientSecret reference appears ONLY in init_script blocks
	// (not env), which means sync cannot safely re-render the env vars without
	// risking secret rotation; sync must be disabled for this app.
	UsedSecretOnlyInInitScript bool
}

// scanAuthSecretEnvScope determines whether the rendered manifest's
// .System.Auth.ClientSecret value lands in env-scoped fields (anywhere
// outside services[*].init_script) or only inside init_script scalars.
//
// The threat model: if the catalog template references the OIDC secret
// only inside init_script (which runs once at install and never again),
// sync would silently rotate the secret on every container recreate
// without delivering the new value to anything that consumes it. The
// running OIDC client persistence still has the OLD hashed secret, OIDC
// auth breaks. The S1' invariant blocks sync for such templates.
//
// Implementation: substitute a unique sentinel string for ClientSecret,
// render the template, parse the rendered YAML, and walk every scalar
// looking for the sentinel. Any sentinel hit outside an init_script
// subtree is "env-scoped"; hits inside init_script are "init-script-scoped".
// This is immune to template authoring tricks ({{ with }}, {{ range }},
// variable assignment, custom funcs, conditional branches) because we look
// at the rendered output, not the template AST.
//
// Returns:
//   - inEnv: sentinel appears in at least one non-init_script scalar
//   - inInitScript: sentinel appears in at least one init_script scalar
//
// If both are false the manifest does not embed the secret at all.
//
// The systemCtx parameter must already contain valid .System.Auth.{Issuer,
// ClientID} values; this function temporarily overrides ClientSecret with
// the sentinel for the scan render and does not mutate the caller's map.
func scanAuthSecretEnvScope(rawTemplate []byte, userInputs map[string]interface{}, systemCtx map[string]interface{}) (inEnv, inInitScript bool, err error) {
	// Build a system context clone with ClientSecret replaced by the sentinel.
	scanCtx := make(map[string]interface{}, len(systemCtx))
	for k, v := range systemCtx {
		if k == "Auth" {
			authClone := map[string]string{}
			if existing, ok := v.(map[string]string); ok {
				for ak, av := range existing {
					authClone[ak] = av
				}
			}
			authClone["ClientSecret"] = authSecretScanSentinel
			scanCtx[k] = authClone
			continue
		}
		scanCtx[k] = v
	}

	rendered, rerr := RenderManifest(rawTemplate, userInputs, scanCtx)
	if rerr != nil {
		// Render failure during scan means the real render will also fail;
		// the caller will detect that and surface the error. Treat as
		// "secret not present" for the scan result; the real render path
		// will halt the pipeline with the actual error.
		return false, false, nil
	}

	var root yaml.Node
	if err := yaml.Unmarshal(rendered, &root); err != nil {
		return false, false, fmt.Errorf("scan secret scope: parse rendered yaml: %w", err)
	}

	// Walk the entire document. Any scalar holding the sentinel that lives
	// outside a services[*].init_script subtree is "env-scoped" — it'll
	// land in container env / config / mount paths and the secret value
	// will be re-injected on every container recreate, which is exactly
	// what sync needs to be safe. References that ONLY appear inside an
	// init_script subtree are dangerous because the script runs once at
	// install and never re-runs on recreate.
	walkScalarsOutsideInitScript(&root, func(scalar string) {
		if strings.Contains(scalar, authSecretScanSentinel) {
			inEnv = true
		}
	})
	walkInitScriptScalars(&root, func(scalar string) {
		if strings.Contains(scalar, authSecretScanSentinel) {
			inInitScript = true
		}
	})
	return inEnv, inInitScript, nil
}

// walkScalarsOutsideInitScript invokes fn on every scalar in the YAML tree
// EXCEPT those nested inside any services[*].init_script mapping.
func walkScalarsOutsideInitScript(node *yaml.Node, fn func(string)) {
	walkScalarsExcluding(node, fn, false, "")
}

// walkInitScriptScalars invokes fn on every scalar nested inside any
// services[*].init_script mapping.
func walkInitScriptScalars(node *yaml.Node, fn func(string)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	services := topLevelValue(node, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		svcVal := services.Content[i+1]
		if svcVal == nil || svcVal.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(svcVal.Content); j += 2 {
			fieldKey := svcVal.Content[j]
			fieldVal := svcVal.Content[j+1]
			if fieldKey == nil || fieldKey.Kind != yaml.ScalarNode {
				continue
			}
			if fieldKey.Value == "init_script" {
				walkAllScalars(fieldVal, fn)
			}
		}
	}
}

// walkScalarsExcluding walks every scalar, recursing into child mapping/sequence
// nodes. If skipInitScriptKey is true on entry it means we are inside a
// services[*] mapping and the walker should skip the value of any init_script
// key. parentKey carries the most recently-seen mapping key for context.
func walkScalarsExcluding(node *yaml.Node, fn func(string), inServiceMapping bool, parentKey string) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			walkScalarsExcluding(child, fn, false, "")
		}
	case yaml.MappingNode:
		// If this mapping is the top-level "services" map, the next level
		// down is per-service mappings — set inServiceMapping for those.
		isServicesTop := parentKey == "services"
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := node.Content[i]
			v := node.Content[i+1]
			if k != nil && k.Kind == yaml.ScalarNode {
				fn(k.Value)
			}
			if isServiceMapping := inServiceMapping; isServiceMapping && k != nil && k.Value == "init_script" {
				continue // skip init_script subtree
			}
			childInService := false
			if isServicesTop {
				// Each value here IS a per-service mapping.
				childInService = true
			} else if inServiceMapping {
				// Already inside a per-service mapping; nested children
				// inherit the in-service flag so init_script under deeper
				// nesting (which doesn't actually exist in the schema, but
				// be defensive) is still skipped if encountered as a key
				// of THIS map. Children of values inherit no flag.
				childInService = false
			}
			walkScalarsExcluding(v, fn, childInService, scalarOrEmpty(k))
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			walkScalarsExcluding(child, fn, false, "")
		}
	case yaml.ScalarNode:
		fn(node.Value)
	}
}

// walkAllScalars walks every scalar in a subtree (no exclusions).
func walkAllScalars(node *yaml.Node, fn func(string)) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		fn(node.Value)
		return
	}
	for _, child := range node.Content {
		walkAllScalars(child, fn)
	}
}

func scalarOrEmpty(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}


// RunInstallPipeline parses, renders, substitutes, validates, and serializes
// an app manifest. It is the single source of truth for the install render
// path; the install handler and the catalog manifest sync path both call this
// function so they cannot drift.
//
// On install: ExistingOIDC is nil; the pipeline generates fresh credentials
// when the manifest declares oidc_client. The caller is responsible for
// persisting the returned credentials via clientMgr.CreateClient AFTER the
// app is installed.
//
// On sync: ExistingOIDC is non-nil; the pipeline uses those credentials
// verbatim and never calls clientGen. clientGen may be nil on sync.
//
// Pipeline steps (numbered to match the plan):
//  1. Parse loose schema
//  2. Build typed runtime systemContext from frozen InstallSystemContext
//  3. Detect OIDC use; obtain credentials (generate or pass-through)
//  4. Inject Auth into systemContext when applicable
//  5. S1' AST scan for ClientSecret env-scope invariant
//  6. Validate user inputs against declared schema
//  7. Render manifest template
//  8. Re-parse rendered output as a loose definition
//  9. __primary substitution (struct edit)
// 10. SetDefaults + ValidateAppDefinition
// 11. Serialize canonical bytes + compute raw template hash
func RunInstallPipeline(ctx context.Context, in InstallPipelineInput, clientGen OIDCClientGenerator, lister AppLister) (*InstallPipelineResult, error) {
	if len(in.RawTemplate) == 0 {
		return nil, fmt.Errorf("install pipeline: empty raw template")
	}

	// Step 1: best-effort pre-render parse. Templates that use block
	// directives ({{ if }}, {{ range }}) to add or remove YAML sections
	// won't parse cleanly until rendered. The old install path treated this
	// as a soft failure: it skipped OIDC pre-detection and input validation
	// when parse failed, then rendered the raw template. Match that
	// behavior so the install pipeline is a drop-in replacement.
	looseSchema, _ := ParseAppSchema(in.RawTemplate)

	// Step 2: build runtime systemContext from frozen snapshot.
	systemContext := map[string]interface{}{
		"Domain":       in.SystemContext.Domain,
		"Architecture": in.SystemContext.Architecture,
		"Timezone":     in.SystemContext.Timezone,
	}

	// Step 3: detect OIDC use and obtain credentials. Only runs when the
	// pre-render parse succeeded — if it didn't, OIDC creds are deferred to
	// the post-render path below (which can re-detect against the rendered
	// definition).
	var creds *OIDCCredentials
	if looseSchema != nil && hasOIDCClient(looseSchema.Services) {
		switch {
		case in.ExistingOIDC != nil:
			c := *in.ExistingOIDC
			creds = &c
		case clientGen != nil:
			id, secret, gerr := clientGen.GenerateCredentials()
			if gerr != nil {
				return nil, fmt.Errorf("install pipeline: generate oidc credentials: %w", gerr)
			}
			creds = &OIDCCredentials{ClientID: id, ClientSecret: secret}
		default:
			return nil, fmt.Errorf("install pipeline: manifest declares oidc_client but no credentials available (clientGen nil and ExistingOIDC nil)")
		}

		// Step 4: inject Auth into render context.
		systemContext["Auth"] = map[string]string{
			"Issuer":       in.SystemContext.IssuerHint,
			"ClientID":     creds.ClientID,
			"ClientSecret": creds.ClientSecret,
		}
	}

	// Step 5: S1' invariant scan via sentinel render. Only meaningful when
	// OIDC is in use; if no oidc_client is declared the scan is a no-op.
	usedSecretOnlyInInitScript := false
	if creds != nil {
		inEnv, inInitScript, scanErr := scanAuthSecretEnvScope(in.RawTemplate, in.UserInputs, systemContext)
		if scanErr != nil {
			return nil, fmt.Errorf("install pipeline: scan secret scope: %w", scanErr)
		}
		usedSecretOnlyInInitScript = !inEnv && inInitScript
	}

	// Step 6: validate user inputs against declared types. Skipped when
	// pre-render parse failed (matches the old install handler's behavior).
	if looseSchema != nil && len(looseSchema.Inputs) > 0 {
		if err := ValidateInputs(looseSchema.Inputs, in.UserInputs); err != nil {
			return nil, fmt.Errorf("install pipeline: %w", err)
		}
	}

	// Step 7: render the template (or skip if no inputs/auth needed).
	rendered := in.RawTemplate
	if len(in.UserInputs) > 0 || creds != nil {
		out, rerr := RenderManifest(in.RawTemplate, in.UserInputs, systemContext)
		if rerr != nil {
			return nil, fmt.Errorf("install pipeline: render manifest: %w", rerr)
		}
		rendered = out
	}

	// Step 8: parse the rendered output back into a loose definition. The
	// rendered output MUST parse — if it doesn't, the manifest is broken.
	def, err := ParseAppSchema(rendered)
	if err != nil {
		return nil, fmt.Errorf("install pipeline: parse rendered: %w", err)
	}

	// If the pre-render parse failed but the post-render def declares OIDC,
	// we missed credential generation in step 3. Re-render with creds and
	// re-parse — this is the rare second-render path for templates with
	// conditional oidc_client blocks.
	if creds == nil && hasOIDCClient(def.Services) {
		switch {
		case in.ExistingOIDC != nil:
			c := *in.ExistingOIDC
			creds = &c
		case clientGen != nil:
			id, secret, gerr := clientGen.GenerateCredentials()
			if gerr != nil {
				return nil, fmt.Errorf("install pipeline: generate oidc credentials: %w", gerr)
			}
			creds = &OIDCCredentials{ClientID: id, ClientSecret: secret}
		default:
			return nil, fmt.Errorf("install pipeline: rendered manifest declares oidc_client but no credentials available")
		}
		systemContext["Auth"] = map[string]string{
			"Issuer":       in.SystemContext.IssuerHint,
			"ClientID":     creds.ClientID,
			"ClientSecret": creds.ClientSecret,
		}
		// S1' scan against the rendered template that now has Auth.
		inEnv, inInitScript, scanErr := scanAuthSecretEnvScope(in.RawTemplate, in.UserInputs, systemContext)
		if scanErr != nil {
			return nil, fmt.Errorf("install pipeline: scan secret scope: %w", scanErr)
		}
		usedSecretOnlyInInitScript = !inEnv && inInitScript
		// Second render with creds injected.
		rendered2, rerr := RenderManifest(in.RawTemplate, in.UserInputs, systemContext)
		if rerr != nil {
			return nil, fmt.Errorf("install pipeline: re-render with creds: %w", rerr)
		}
		def, err = ParseAppSchema(rendered2)
		if err != nil {
			return nil, fmt.Errorf("install pipeline: parse re-rendered: %w", err)
		}
	}

	// Step 9: __primary substitution. Mirrors gin_app_handlers.go install path.
	hasPrimaryMarker := false
	for i := range def.Listeners {
		if !hostname.IsPrimaryMarker(def.Listeners[i].Name) {
			continue
		}
		hasPrimaryMarker = true
		appAddress, _ := in.UserInputs["__app_address__"].(string)
		appAddress = strings.TrimSpace(appAddress)
		if appAddress == "" && in.InstanceID != "" {
			appAddress = in.InstanceID
		}
		if appAddress == "" {
			return nil, fmt.Errorf("install pipeline: app requires '__app_address__' input for primary listener name")
		}
		if err := ValidateInstanceID(appAddress); err != nil {
			return nil, fmt.Errorf("install pipeline: invalid app address: %w", err)
		}
		if lister != nil {
			existing, lerr := lister.List(ctx)
			if lerr != nil {
				return nil, fmt.Errorf("install pipeline: list existing apps: %w", lerr)
			}
			ids := make([]string, 0, len(existing))
			for _, a := range existing {
				if a.InstanceID == in.InstanceID {
					continue // skip self on sync re-render
				}
				ids = append(ids, a.InstanceID)
			}
			if err := ValidatePrimaryNameAvailable(appAddress, ids); err != nil {
				return nil, fmt.Errorf("install pipeline: %w", err)
			}
		}
		def.Listeners[i].Name = appAddress
		def.Listeners[i].Primary = true
		break
	}
	if !hasPrimaryMarker && len(def.Listeners) > 0 {
		return nil, fmt.Errorf("install pipeline: apps with listeners must have exactly one listener named '__primary'")
	}

	// Workspace identity: substitute __app_address__ into workspace_name when applicable.
	if len(def.Listeners) == 0 {
		appAddr, _ := in.UserInputs["__app_address__"].(string)
		appAddr = strings.TrimSpace(appAddr)
		wsName := def.WorkspaceName
		if appAddr != "" {
			wsName = appAddr
		}
		if wsName != "" {
			if err := ValidateInstanceID(wsName); err != nil {
				return nil, fmt.Errorf("install pipeline: invalid workspace name: %w", err)
			}
			if lister != nil {
				existing, lerr := lister.List(ctx)
				if lerr != nil {
					return nil, fmt.Errorf("install pipeline: list existing apps: %w", lerr)
				}
				ids := make([]string, 0, len(existing))
				for _, a := range existing {
					if a.InstanceID == in.InstanceID {
						continue
					}
					ids = append(ids, a.InstanceID)
				}
				if err := ValidatePrimaryNameAvailable(wsName, ids); err != nil {
					return nil, fmt.Errorf("install pipeline: %w", err)
				}
			}
			def.WorkspaceName = wsName
		}
	}

	// Step 10: SetDefaults then full validation.
	SetDefaults(def)
	if err := ValidateAppDefinition(def); err != nil {
		return nil, fmt.Errorf("install pipeline: validate definition: %w", err)
	}

	// Step 11: canonical serialization + raw template hash.
	canonical, err := SerializeAppDefinition(def)
	if err != nil {
		return nil, fmt.Errorf("install pipeline: serialize canonical: %w", err)
	}

	return &InstallPipelineResult{
		Definition:                 def,
		OIDCCredentials:            creds,
		CanonicalBytes:             canonical,
		RawTemplateHash:            Sha256Hex(in.RawTemplate),
		UsedSecretOnlyInInitScript: usedSecretOnlyInInitScript,
	}, nil
}

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"piccolod/internal/api"
	"piccolod/internal/container"
	"piccolod/internal/hostname"
	"piccolod/internal/resources"

	"gopkg.in/yaml.v3"
)

const (
	// Safe markers for Go template delimiters during YAML parsing.
	tplLeftMarker  = "__TPL_L__"
	tplRightMarker = "__TPL_R__"
)

// jsonValue wraps a value so that its String() method produces JSON.
// Used to make slice/array template inputs render as valid YAML automatically
// (e.g., {{ .Inputs.list }} → ["a","b"] instead of Go's default [a b]).
type jsonValue struct{ v any }

func (j jsonValue) String() string {
	b, _ := json.Marshal(j.v)
	return string(b)
}

func (j jsonValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(j.v)
}

// manifestFuncMap provides custom template functions for app manifest rendering.
var manifestFuncMap = template.FuncMap{
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

// wrapSliceInputs wraps []interface{} values in jsonValue so that Go templates
// render them as JSON arrays (valid YAML) instead of Go's fmt.Sprint format.
// Scalar values (string, bool, number) are left untouched.
func wrapSliceInputs(inputs map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(inputs))
	for k, v := range inputs {
		if _, ok := v.([]interface{}); ok {
			out[k] = jsonValue{v}
		} else {
			out[k] = v
		}
	}
	return out
}

var (
	// Valid app name pattern: lowercase letters, numbers, hyphens
	// Must start with letter, end with letter or number
	appNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

	// initShellRegex restricts init_script.shell to a safe character set:
	// alphanumerics, underscore, slash, dot, hyphen. Prevents whitespace,
	// shell metacharacters, and control characters.
	initShellRegex = regexp.MustCompile(`^[A-Za-z0-9_/.-]+$`)

	// tplExprRegex matches Go template expressions {{ ... }}.
	// Unquoted {{ }} in YAML is interpreted as a flow mapping, which breaks
	// yaml.Unmarshal for template expressions like `key: {{ .Inputs.x }}`.
	// We target complete expressions (not bare {{ or }}) to avoid breaking
	// legitimate YAML flow mappings like {key: {nested: val}}.
	tplExprRegex = regexp.MustCompile(`\{\{.*?\}\}`)
)

// neutralizeTemplates replaces Go template delimiters within {{ ... }} expressions
// with safe markers so unquoted expressions don't break YAML parsing.
func neutralizeTemplates(content []byte) []byte {
	return tplExprRegex.ReplaceAllFunc(content, func(match []byte) []byte {
		result := bytes.ReplaceAll(match, []byte("{{"), []byte(tplLeftMarker))
		result = bytes.ReplaceAll(result, []byte("}}"), []byte(tplRightMarker))
		return result
	})
}

// restoreTemplatesInNode walks a yaml.Node tree and restores Go template
// delimiters from safe markers in all scalar values.
func restoreTemplatesInNode(node *yaml.Node) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode {
		node.Value = strings.ReplaceAll(node.Value, tplLeftMarker, "{{")
		node.Value = strings.ReplaceAll(node.Value, tplRightMarker, "}}")
		return
	}
	for _, child := range node.Content {
		restoreTemplatesInNode(child)
	}
}

// PiccoloMode represents the execution mode for an app as defined in x-piccolo.mode.
type PiccoloMode string

const (
	// ModeService indicates a stateless/replaceable container mode.
	// Container filesystem is ephemeral and not preserved across reinstalls.
	ModeService PiccoloMode = "service"

	// ModeWorkspace indicates a persistent workspace mode.
	// Container filesystem is backed by a persistent workspace disk.
	ModeWorkspace PiccoloMode = "workspace"

	// ModeUnknown indicates the mode could not be determined.
	ModeUnknown PiccoloMode = ""
)

func hasTopLevelKey(node *yaml.Node, key string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return true
		}
	}
	return false
}

func topLevelMapping(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func topLevelValue(node *yaml.Node, key string) *yaml.Node {
	m := topLevelMapping(node)
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		v := m.Content[i+1]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return v
		}
	}
	return nil
}

// validateRawListenersNoPrimary validates that no listener specifies the 'primary' field
// in user-authored manifests. Per RFC 20260130, the primary listener is determined by
// the __primary name marker, not by a primary: true field.
//
// However, the 'primary' field IS allowed in persisted/loaded app definitions where the
// __primary marker has already been substituted with the actual app name.
func validateRawListenersNoPrimary(root *yaml.Node) error {
	listeners := topLevelValue(root, "listeners")
	if listeners == nil {
		return nil
	}
	if listeners.Kind != yaml.SequenceNode {
		return nil // Let the decoder handle the type error
	}

	// First pass: check if any listener has the __primary marker
	// If so, this is a pre-substitution manifest and NO listener should have primary: true
	hasPrimaryMarkerAnywhere := false
	for _, item := range listeners.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			keyNode := item.Content[j]
			valueNode := item.Content[j+1]
			if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
				continue
			}
			if keyNode.Value == "name" && valueNode != nil && valueNode.Value == "__primary" {
				hasPrimaryMarkerAnywhere = true
				break
			}
		}
		if hasPrimaryMarkerAnywhere {
			break
		}
	}

	// Second pass: if this is a pre-substitution manifest, reject any primary: true field
	if hasPrimaryMarkerAnywhere {
		for i, item := range listeners.Content {
			if item == nil || item.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(item.Content); j += 2 {
				keyNode := item.Content[j]
				if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
					continue
				}
				if keyNode.Value == "primary" {
					return fmt.Errorf("listeners[%d].primary is not allowed in YAML; use the '__primary' listener name instead", i)
				}
			}
		}
	}
	return nil
}

func validateRawServicesBlocks(root *yaml.Node) error {
	services := topLevelValue(root, "services")
	if services == nil {
		return nil
	}
	if services.Kind != yaml.MappingNode {
		return fmt.Errorf("services must be a mapping")
	}

	allowedServiceKeys := map[string]struct{}{
		"image":       {},
		"init":        {},
		"init_script": {},
		"after":       {},
		"bind_ports":  {},
		"environment": {},
		"storage":     {},
		"oidc_client": {},
	}

	for i := 0; i+1 < len(services.Content); i += 2 {
		svcKey := services.Content[i]
		svcVal := services.Content[i+1]
		if svcKey == nil || svcKey.Kind != yaml.ScalarNode {
			continue
		}
		name := strings.TrimSpace(svcKey.Value)
		if svcVal == nil || svcVal.Kind != yaml.MappingNode {
			return fmt.Errorf("services.%s must be a mapping", name)
		}
		for j := 0; j+1 < len(svcVal.Content); j += 2 {
			fieldKey := svcVal.Content[j]
			if fieldKey == nil || fieldKey.Kind != yaml.ScalarNode {
				continue
			}
			field := strings.TrimSpace(fieldKey.Value)
			if field == "wait_for" {
				return fmt.Errorf("services.%s.wait_for is not supported yet", name)
			}
			if field == "resources" {
				return fmt.Errorf("services.%s.resources is no longer supported (pre-v2 resource-stewardship schema); the resources block now lives at the app top level. Run 'piccolod catalog sync' to fetch an updated manifest.", name)
			}
			if _, ok := allowedServiceKeys[field]; !ok {
				return fmt.Errorf("services.%s contains unsupported field '%s'", name, field)
			}
		}
	}

	return nil
}

// IsLegacyResourcesSchemaError reports whether err came from the pre-v2
// resource-stewardship raw-YAML rejection path (validateRawResourcesShape
// or validateRawServicesNoResources). Callers use this to trigger the
// one-shot legacy→new-shape migration on persisted app.yaml loads.
func IsLegacyResourcesSchemaError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "pre-v2 resource-stewardship schema")
}

// MigrateLegacyResourcesYAML rewrites persisted legacy-shape app.yaml
// into new-shape form by stripping pre-v2 resource declarations. The
// stripped manifest parses cleanly under the new schema with no
// resources block (kernel defaults apply). The next catalog sync picks
// up the properly authored new-shape manifest and re-derives the slice
// policy.
//
// This is strictly a safety-net for the upgrade path on the deployed
// N=1 user's existing immich install. Steady-state behaviour is
// governed by the catalog re-author (Phase 2.4).
//
// Returns the migrated YAML content and true if any legacy fields were
// present; returns the original bytes and false if nothing needed
// rewriting.
func MigrateLegacyResourcesYAML(content []byte) ([]byte, bool, error) {
	safe := neutralizeTemplates(content)
	var root yaml.Node
	if err := yaml.Unmarshal(safe, &root); err != nil {
		return content, false, fmt.Errorf("migrate: parse yaml: %w", err)
	}
	changed := false
	// Strip top-level resources.limits (preserve other resources subkeys
	// if any — none exist in legacy form, but the scan is defensive).
	if rn := topLevelValue(&root, "resources"); rn != nil && rn.Kind == yaml.MappingNode {
		var keep []*yaml.Node
		for i := 0; i+1 < len(rn.Content); i += 2 {
			k := rn.Content[i]
			v := rn.Content[i+1]
			if k != nil && k.Kind == yaml.ScalarNode && k.Value == "limits" {
				changed = true
				continue
			}
			keep = append(keep, k, v)
		}
		rn.Content = keep
		// If the resources block is now empty, remove it from the parent map.
		if len(rn.Content) == 0 {
			if m := topLevelMapping(&root); m != nil {
				var nkeep []*yaml.Node
				for i := 0; i+1 < len(m.Content); i += 2 {
					k := m.Content[i]
					if k != nil && k.Kind == yaml.ScalarNode && k.Value == "resources" {
						continue
					}
					nkeep = append(nkeep, m.Content[i], m.Content[i+1])
				}
				m.Content = nkeep
			}
		}
	}
	// Strip per-service resources blocks.
	if svcs := topLevelValue(&root, "services"); svcs != nil && svcs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(svcs.Content); i += 2 {
			svcVal := svcs.Content[i+1]
			if svcVal == nil || svcVal.Kind != yaml.MappingNode {
				continue
			}
			var keep []*yaml.Node
			for j := 0; j+1 < len(svcVal.Content); j += 2 {
				fk := svcVal.Content[j]
				if fk != nil && fk.Kind == yaml.ScalarNode && fk.Value == "resources" {
					changed = true
					continue
				}
				keep = append(keep, svcVal.Content[j], svcVal.Content[j+1])
			}
			svcVal.Content = keep
		}
	}
	if !changed {
		return content, false, nil
	}
	restoreTemplatesInNode(&root)
	migrated, err := yaml.Marshal(&root)
	if err != nil {
		return content, false, fmt.Errorf("migrate: re-marshal yaml: %w", err)
	}
	return migrated, true, nil
}

// validateRawServicesNoResources rejects per-service `resources` blocks at
// the raw-YAML stage. Called from both ParseAppSchema (loose parse) and
// ParseAppDefinition (full parse) so the legacy shape is consistently
// rejected regardless of entry point. See plans/resource-stewardship.md D-6.
func validateRawServicesNoResources(root *yaml.Node) error {
	services := topLevelValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(services.Content); i += 2 {
		svcKey := services.Content[i]
		svcVal := services.Content[i+1]
		if svcKey == nil || svcKey.Kind != yaml.ScalarNode {
			continue
		}
		if svcVal == nil || svcVal.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(svcVal.Content); j += 2 {
			fieldKey := svcVal.Content[j]
			if fieldKey == nil || fieldKey.Kind != yaml.ScalarNode {
				continue
			}
			if fieldKey.Value == "resources" {
				return fmt.Errorf("services.%s.resources is no longer supported (pre-v2 resource-stewardship schema); the resources block now lives at the app top level. Run 'piccolod catalog sync' to fetch an updated manifest.", svcKey.Value)
			}
		}
	}
	return nil
}

// validateRawResourcesShape rejects pre-v2 resource-stewardship schema at the
// raw-YAML stage. Triggers a hard parse error with a catalog-sync hint when:
//   - top-level resources.limits is present (legacy shape pre-RFC)
//
// Per-service resources blocks are rejected by validateRawServicesNoResources.
// See plans/resource-stewardship.md D-6.
func validateRawResourcesShape(root *yaml.Node) error {
	resources := topLevelValue(root, "resources")
	if resources == nil || resources.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(resources.Content); i += 2 {
		k := resources.Content[i]
		if k == nil || k.Kind != yaml.ScalarNode {
			continue
		}
		if k.Value == "limits" {
			return fmt.Errorf("resources.limits is no longer supported (pre-v2 resource-stewardship schema); declare priority/memory/storage at the resources top level. Run 'piccolod catalog sync' to fetch an updated manifest.")
		}
	}
	return nil
}

// ParseAppSchema parses the YAML to extract metadata and inputs without strict validation.
// This is used to read the manifest before variable substitution.
func ParseAppSchema(content []byte) (*api.AppDefinition, error) {
	// Neutralize Go template expressions so unquoted {{ }} (which YAML interprets
	// as flow mappings) don't break parsing. Restored after parse, before decode.
	safe := neutralizeTemplates(content)

	var root yaml.Node
	if err := yaml.Unmarshal(safe, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if hasTopLevelKey(&root, "filesystem") {
		return nil, fmt.Errorf("filesystem is no longer supported; use x-piccolo.mode")
	}
	if hasTopLevelKey(&root, "build") {
		return nil, fmt.Errorf("build is not supported; specify image")
	}
	if hasTopLevelKey(&root, "depends_on") {
		return nil, fmt.Errorf("depends_on is not supported; dependencies must be packaged as sidecars")
	}
	// RFC 20260130: Reject primary field in listener YAML even during schema parsing
	if err := validateRawListenersNoPrimary(&root); err != nil {
		return nil, err
	}
	// Resource-stewardship: reject pre-v2 resource schema at the raw-YAML
	// stage in all parse paths (both ParseAppSchema and ParseAppDefinition).
	// Legacy shapes silently drop through YAML decode otherwise, because the
	// new AppResources struct has no `limits` field and unknown keys are
	// ignored by the gopkg.in/yaml.v3 decoder. See plans/resource-stewardship.md D-6.
	if err := validateRawResourcesShape(&root); err != nil {
		return nil, err
	}
	if err := validateRawServicesNoResources(&root); err != nil {
		return nil, err
	}

	// Restore template delimiters before decoding so struct fields
	// (e.g., input defaults with {{ .System.* }}) contain the original template text.
	restoreTemplatesInNode(&root)

	var app api.AppDefinition
	if err := root.Decode(&app); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	// We do NOT call SetDefaults or ValidateAppDefinition here because
	// fields like listener names might contain "{{ .Inputs... }}" which would fail validation.
	return &app, nil
}

// backfillInputDefaults ensures every declared optional input has a key in the
// provided map. For optional inputs (required: false) not provided by the user,
// it injects the declared default value or a type-appropriate zero value.
// This prevents missingkey=error panics when templates reference optional inputs.
func backfillInputDefaults(declared map[string]api.AppInput, provided map[string]interface{}) map[string]interface{} {
	if len(declared) == 0 {
		return provided
	}
	// Check if any optional input needs backfilling before allocating.
	needsBackfill := false
	for name, spec := range declared {
		if _, exists := provided[name]; !exists && !spec.Required {
			needsBackfill = true
			break
		}
	}
	if !needsBackfill {
		return provided
	}
	merged := make(map[string]interface{}, len(provided)+len(declared))
	for k, v := range provided {
		merged[k] = v
	}
	for name, spec := range declared {
		if _, exists := merged[name]; exists {
			continue
		}
		if spec.Required {
			continue // let missingkey=error catch missing required inputs
		}
		if spec.Default != nil {
			merged[name] = spec.Default
			continue
		}
		switch spec.Type {
		case "boolean":
			merged[name] = false
		case "int", "number":
			merged[name] = float64(0) // float64 matches JSON/YAML number decoding
		case "array":
			merged[name] = []interface{}{}
		default: // string, password
			merged[name] = ""
		}
	}
	return merged
}

// ValidateInputs validates user-provided inputs against their declared types and constraints.
// Returns an error listing all validation failures, or nil if all inputs are valid.
// Unknown types are silently accepted for forward compatibility.
func ValidateInputs(declared map[string]api.AppInput, provided map[string]interface{}) error {
	var errs []string
	for name, spec := range declared {
		val, exists := provided[name]
		if !exists || val == nil {
			if spec.Required && !spec.Generate {
				errs = append(errs, fmt.Sprintf("input '%s' is required", name))
			}
			continue
		}
		if err := validateInputValue(name, spec, val); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("input validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateInputValue(name string, spec api.AppInput, val interface{}) error {
	switch spec.Type {
	case "string", "password":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("input '%s': expected string, got %T", name, val)
		}
		return validateRegex(name, spec, s)

	case "boolean":
		switch val.(type) {
		case bool:
			// ok
		case string:
			v := val.(string)
			if v != "true" && v != "false" {
				return fmt.Errorf("input '%s': expected boolean, got string %q", name, v)
			}
		default:
			return fmt.Errorf("input '%s': expected boolean, got %T", name, val)
		}

	case "int":
		f, ok := val.(float64)
		if !ok {
			return fmt.Errorf("input '%s': expected number, got %T", name, val)
		}
		if f != float64(int64(f)) {
			return fmt.Errorf("input '%s': expected integer, got %v", name, f)
		}

	case "number":
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("input '%s': expected number, got %T", name, val)
		}

	case "array":
		arr, ok := val.([]interface{})
		if !ok {
			return fmt.Errorf("input '%s': expected array, got %T", name, val)
		}
		var re *regexp.Regexp
		if spec.Validation != nil && spec.Validation.Regex != "" {
			var err error
			re, err = regexp.Compile(spec.Validation.Regex)
			if err != nil {
				return fmt.Errorf("input '%s': invalid regex pattern: %v", name, err)
			}
		}
		for i, elem := range arr {
			s, ok := elem.(string)
			if !ok {
				return fmt.Errorf("input '%s[%d]': expected string element, got %T", name, i, elem)
			}
			if re != nil && !re.MatchString(s) {
				msg := spec.Validation.Message
				if msg == "" {
					msg = "does not match pattern"
				}
				return fmt.Errorf("input '%s[%d]': %s", name, i, msg)
			}
		}

	default:
		// Unknown type — skip validation for forward compatibility.
	}
	return nil
}

func validateRegex(name string, spec api.AppInput, val string) error {
	if spec.Validation == nil || spec.Validation.Regex == "" {
		return nil
	}
	re, err := regexp.Compile(spec.Validation.Regex)
	if err != nil {
		return fmt.Errorf("input '%s': invalid regex pattern: %v", name, err)
	}
	if !re.MatchString(val) {
		msg := spec.Validation.Message
		if msg == "" {
			msg = "does not match pattern"
		}
		return fmt.Errorf("input '%s': %s", name, msg)
	}
	return nil
}

// RenderManifest applies user inputs to the app manifest template.
func RenderManifest(rawYaml []byte, userInputs map[string]interface{}, systemContext map[string]interface{}) ([]byte, error) {
	// Backfill zero values for declared-but-unprovided optional inputs
	// so templates can use {{ .Inputs.x }} without missingkey=error panics.
	if schema, err := ParseAppSchema(rawYaml); err == nil && len(schema.Inputs) > 0 {
		userInputs = backfillInputDefaults(schema.Inputs, userInputs)
	}

	// Prepare data for template.
	// wrapSliceInputs ensures array values render as JSON (valid YAML)
	// without requiring explicit | toJSON in templates.
	data := map[string]interface{}{
		"Inputs": wrapSliceInputs(userInputs),
		"System": systemContext,
	}

	// Parse and Execute Template
	// Option "missingkey=error" ensures we fail if the manifest references an input we didn't provide
	tmpl, err := template.New("manifest").Funcs(manifestFuncMap).Option("missingkey=error").Parse(string(rawYaml))
	if err != nil {
		return nil, fmt.Errorf("failed to parse manifest template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("failed to execute manifest template: %w", err)
	}

	return buf.Bytes(), nil
}

// ParseAppDefinition parses YAML content into AppDefinition struct with validation
func ParseAppDefinition(content []byte) (*api.AppDefinition, error) {
	var app api.AppDefinition

	// Parse YAML
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if hasTopLevelKey(&root, "filesystem") {
		return nil, fmt.Errorf("filesystem is no longer supported; use x-piccolo.mode")
	}
	if hasTopLevelKey(&root, "build") {
		return nil, fmt.Errorf("build is not supported; specify image")
	}
	if hasTopLevelKey(&root, "depends_on") {
		return nil, fmt.Errorf("depends_on is not supported; dependencies must be packaged as sidecars")
	}
	if err := validateRawServicesBlocks(&root); err != nil {
		return nil, err
	}
	if err := validateRawResourcesShape(&root); err != nil {
		return nil, err
	}
	// RFC 20260130: Reject primary field in listener YAML - it's set programmatically only
	if err := validateRawListenersNoPrimary(&root); err != nil {
		return nil, err
	}
	if err := root.Decode(&app); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	// Set defaults
	SetDefaults(&app)

	// Validate
	if err := ValidateAppDefinition(&app); err != nil {
		return nil, err
	}

	return &app, nil
}

// SerializeAppDefinition serializes AppDefinition to YAML bytes
func SerializeAppDefinition(app *api.AppDefinition) ([]byte, error) {
	data, err := yaml.Marshal(app)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal YAML: %w", err)
	}
	return data, nil
}

// SetDefaults sets default values for AppDefinition fields
func SetDefaults(app *api.AppDefinition) {
	// Default type is "user"
	if app.Type == "" {
		app.Type = "user"
	}

	// Listeners defaults
	for i := range app.Listeners {
		if app.Listeners[i].Flow == api.FlowUnknown {
			app.Listeners[i].Flow = api.FlowTCP
		}
		if app.Listeners[i].Protocol == api.ListenerProtocolUnknown {
			// RFC 20260130: flow:tcp primary listeners require host-based routing, so default to http.
			// flow:tls and flow:udp primary listeners don't require HTTP semantics, so default to raw.
			// Non-primary listeners default to raw for backward compatibility.
			isPrimary := app.Listeners[i].Primary || hostname.IsPrimaryMarker(app.Listeners[i].Name)
			if isPrimary && app.Listeners[i].Flow == api.FlowTCP {
				app.Listeners[i].Protocol = api.ListenerProtocolHTTP
			} else {
				app.Listeners[i].Protocol = api.ListenerProtocolRaw
			}
		}
	}
}

// validateAppIdentity validates the app identity configuration per RFC 20260130.
// Apps must provide identity via exactly one of:
// - listeners with exactly one __primary (apps with network access)
// - workspace_name (workspace apps without listeners)
//
// RFC 20260130 §10.1: Workspace mode apps can evolve to have both workspace_name
// and listeners. This occurs when listeners are added to a workspace that was
// originally installed with workspace_name. The instanceID remains workspace_name
// (accepted asymmetry). Service mode apps still require mutual exclusivity.
func validateAppIdentity(app *api.AppDefinition, mode PiccoloMode) error {
	hasListeners := len(app.Listeners) > 0
	hasWorkspaceName := strings.TrimSpace(app.WorkspaceName) != ""

	// workspace_name format validation (if present)
	if hasWorkspaceName {
		if err := hostname.ValidateWorkspaceName(app.WorkspaceName); err != nil {
			return err
		}
	}

	// RFC 20260130 §10.1: Workspace mode apps can have both workspace_name and listeners
	// (evolved workspace). Service mode requires mutual exclusivity.
	if hasListeners && hasWorkspaceName {
		if mode != ModeWorkspace {
			return fmt.Errorf("workspace_name cannot be used with listeners; the primary listener name becomes the app identity")
		}
		// Workspace mode with both: valid (evolved workspace per §10.1)
		return nil
	}

	// workspace_name without listeners is only valid for workspace mode
	if hasWorkspaceName && !hasListeners {
		if mode != ModeWorkspace {
			return fmt.Errorf("workspace_name is only allowed for workspace mode apps")
		}
	}

	// Apps without listeners must have workspace_name (workspace mode only)
	if !hasListeners && !hasWorkspaceName {
		if mode == ModeWorkspace {
			return fmt.Errorf("workspace_name is required for workspace apps without listeners")
		}
		// RFC 20260130: Service mode apps require listeners for identity
		if mode == ModeService {
			return fmt.Errorf("listeners are required for service mode apps")
		}
		// For unknown mode, return generic error
		return fmt.Errorf("listeners or workspace_name is required for app identity")
	}

	return nil
}

// ValidateAppDefinition validates an AppDefinition struct
func ValidateAppDefinition(app *api.AppDefinition) error {
	// Validate type
	if err := validateType(app.Type); err != nil {
		return err
	}

	// RFC 20260112: App-level auth block is removed.
	if app.Auth != nil {
		return newValidationError("APP_AUTH_DEPRECATED", "app-level auth block is deprecated; use listeners[].auth and services[].oidc_client")
	}

	// Validate Piccolo-specific extensions (required mode + consistency checks).
	// We validate this early because mode impacts several other validations.
	if err := validatePiccoloExtensions(app); err != nil {
		return err
	}
	mode := piccoloModeFromExtensions(app.Extensions)

	// RFC 20260130: Validate app identity configuration.
	// Apps must provide identity via exactly one of:
	// - listeners with exactly one __primary (service/workspace apps with network access)
	// - workspace_name (workspace apps without listeners)
	if err := validateAppIdentity(app, mode); err != nil {
		return err
	}

	// Validate container model (single vs multi-container) and disallowed fields.
	if err := validateContainerModel(app, mode); err != nil {
		return err
	}

	// Validate listeners (service-oriented)
	// Workspace mode apps are allowed to have no listeners
	if err := validateListeners(app.Listeners, mode); err != nil {
		return err
	}

	// Validate storage
	if err := validateStorage(app.Storage); err != nil {
		return err
	}

	// Validate resources
	if err := validateResources(app.Resources); err != nil {
		return err
	}

	// Validate permissions
	if err := validatePermissions(app.Permissions); err != nil {
		return err
	}

	// RFC 20260112: oidc_passthrough requires at least one service to declare oidc_client.
	if usesOIDCPassthrough(app.Listeners) && !hasOIDCClient(app.Services) {
		return newValidationError("OIDC_CLIENT_REQUIRED", "oidc_passthrough strategy requires at least one service to declare oidc_client")
	}

	return nil
}

func piccoloModeFromExtensions(extensions map[string]interface{}) PiccoloMode {
	if extensions == nil {
		return ModeUnknown
	}
	raw, ok := extensions["mode"]
	if !ok {
		return ModeUnknown
	}
	modeStr, ok := raw.(string)
	if !ok {
		return ModeUnknown
	}
	modeStr = strings.TrimSpace(modeStr)
	switch PiccoloMode(modeStr) {
	case ModeService:
		return ModeService
	case ModeWorkspace:
		return ModeWorkspace
	default:
		return ModeUnknown
	}
}

func validatePiccoloExtensions(app *api.AppDefinition) error {
	if app == nil {
		return nil
	}

	mode := piccoloModeFromExtensions(app.Extensions)
	if mode == ModeUnknown {
		if app.Extensions == nil {
			return fmt.Errorf("x-piccolo is required; set x-piccolo.mode to 'service' or 'workspace'")
		}
		// Check if mode key exists but has invalid value
		if raw, ok := app.Extensions["mode"]; ok {
			return fmt.Errorf("x-piccolo.mode must be 'service' or 'workspace', got '%v'", raw)
		}
		return fmt.Errorf("x-piccolo.mode is required; must be 'service' or 'workspace'")
	}

	return nil
}

const defaultPrimaryServiceName = "main"

func validateContainerModel(app *api.AppDefinition, mode PiccoloMode) error {
	if app == nil {
		return nil
	}

	switch mode {
	case ModeService:
		if len(app.Services) == 0 {
			return fmt.Errorf("services is required for service mode apps")
		}

		// Service-mode apps must define container fields per-service only.
		if strings.TrimSpace(app.Image) != "" {
			return fmt.Errorf("image must be specified per-service under services for service mode apps")
		}
		if app.Environment != nil {
			return fmt.Errorf("environment must be specified per-service under services for service mode apps")
		}
		if app.Storage != nil {
			return fmt.Errorf("storage must be specified per-service under services for service mode apps")
		}
		// app.Resources (resource-stewardship) IS valid at the app level for service
		// mode — the block enforces priority/memory/storage at the per-app-user slice
		// covering all services collectively. Per-service resources are rejected at
		// the raw-YAML stage. Field validation happens via validateResources called
		// from ValidateAppDefinition before we reach this mode-specific switch.
		for name, svc := range app.Services {
			if svc.Init == "image" {
				return fmt.Errorf("services.%s.init: 'image' is only valid for workspace mode apps", name)
			}
		}

		primary := strings.TrimSpace(app.PrimaryService)
		if primary == "" {
			primary = defaultPrimaryServiceName
			if len(app.Services) == 1 {
				for name := range app.Services {
					primary = name
				}
			}
		}
		if _, ok := app.Services[primary]; !ok {
			return fmt.Errorf("primary_service '%s' not found in services", primary)
		}

		return validateServices(app.Services, primary, app.Listeners)
	case ModeWorkspace:
		if len(app.Services) == 0 {
			return fmt.Errorf("services is required for workspace mode apps")
		}
		if len(app.Services) != 1 {
			return fmt.Errorf("workspace mode apps must define exactly one service")
		}
		// Workspace-mode apps must define container fields per-service only.
		if strings.TrimSpace(app.Image) != "" {
			return fmt.Errorf("image must be specified per-service under services for workspace mode apps")
		}
		if app.Environment != nil {
			return fmt.Errorf("environment must be specified per-service under services for workspace mode apps")
		}
		if app.Storage != nil {
			return fmt.Errorf("storage must be specified per-service under services for workspace mode apps")
		}
		// app.Resources (resource-stewardship) IS valid at the app level for workspace
		// mode — same semantics as service mode. Field validation happens via
		// validateResources called from ValidateAppDefinition.

		for name, svc := range app.Services {
			if svc.InitScript != nil {
				return fmt.Errorf("services.%s.init_script is not supported for workspace mode apps", name)
			}
		}

		primary := strings.TrimSpace(app.PrimaryService)
		if primary == "" {
			primary = defaultPrimaryServiceName
		}
		if _, ok := app.Services[primary]; !ok {
			return fmt.Errorf("primary_service '%s' not found in services", primary)
		}
		return validateServices(app.Services, primary, app.Listeners)
	}

	return nil
}

func validateServiceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("service name cannot be empty")
	}
	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("service name '%s' must contain only lowercase letters, numbers, and hyphens, and must start with a letter", name)
	}
	// Reject "--" which is used as the per-service rootfs volume ID delimiter.
	if strings.Contains(name, "--") {
		return fmt.Errorf("service name '%s' must not contain consecutive hyphens", name)
	}
	return nil
}

func validateServices(services map[string]api.AppService, primary string, listeners []api.AppListener) error {
	oidcEnv := make(map[string]string)

	// Validate service specs first (names, images, per-service storage/resources).
	for name, svc := range services {
		if err := validateServiceName(name); err != nil {
			return err
		}
		if strings.TrimSpace(svc.Image) == "" {
			return fmt.Errorf("services.%s.image is required", name)
		}
		if svc.Init != "" && svc.Init != "image" {
			return fmt.Errorf("services.%s.init must be 'image' or omitted, got '%s'", name, svc.Init)
		}
		// bind_ports is required (may be empty) so Piccolo can validate shared port namespace collisions.
		if svc.BindPorts == nil {
			return fmt.Errorf("services.%s.bind_ports is required (may be empty)", name)
		}
		// Per-service resources blocks are no longer accepted. The resource-stewardship
		// schema places all memory/CPU/storage declarations at the app level. Detection
		// of legacy per-service resources blocks happens at the raw-YAML scan stage
		// (see validateRawServicesNoResources in this file).
		if err := validateStorage(svc.Storage); err != nil {
			return fmt.Errorf("services.%s.storage invalid: %w", name, err)
		}

		if svc.OIDCClient != nil {
			if len(svc.OIDCClient.Env) == 0 {
				return newValidationError("OIDC_ENV_REQUIRED", "oidc_client.env must not be empty")
			}
			if strings.TrimSpace(svc.OIDCClient.CAMountPath) == "" {
				return newValidationError("OIDC_CA_PATH_REQUIRED", "oidc_client.ca_mount_path is required")
			}
			if len(svc.OIDCClient.RedirectURIPaths) == 0 {
				return newValidationError("OIDC_REDIRECT_PATHS_REQUIRED", "oidc_client.redirect_uri_paths must be a non-empty list")
			}
			for _, p := range svc.OIDCClient.RedirectURIPaths {
				if err := validateOIDCRedirectPath(p); err != nil {
					return newValidationError("INVALID_REDIRECT_PATH", err.Error())
				}
			}
			for k, v := range svc.OIDCClient.Env {
				if prior, ok := oidcEnv[k]; ok && prior != v {
					return fmt.Errorf("oidc_client.env conflicts for key '%s' across services", k)
				}
				oidcEnv[k] = v
			}
			for _, redirectURI := range svc.OIDCClient.RedirectURIs {
				if err := validateOIDCRedirectURI(redirectURI); err != nil {
					return newValidationError("INVALID_REDIRECT_URI", fmt.Sprintf("redirect_uri \"%s\" must be localhost, loopback (127.0.0.1, ::1), or custom scheme", redirectURI))
				}
			}
		}

		if err := validateInitScript(name, svc.InitScript); err != nil {
			return err
		}
	}

	// Validate after graph (unknown refs + cycles) and ensure stable order exists.
	if _, err := serviceStartOrder(services); err != nil {
		return err
	}

	// Validate bind_ports across shared network namespace.
	portOwners := make(map[int]string)
	for name, svc := range services {
		seen := make(map[int]struct{}, len(svc.BindPorts))
		for _, port := range svc.BindPorts {
			if port < 1 || port > 65535 {
				return fmt.Errorf("services.%s.bind_ports contains invalid port %d", name, port)
			}
			if _, ok := seen[port]; ok {
				return fmt.Errorf("services.%s.bind_ports contains duplicate port %d", name, port)
			}
			seen[port] = struct{}{}
			if other, ok := portOwners[port]; ok {
				return fmt.Errorf("bind_ports collision: port %d declared by both services '%s' and '%s'", port, other, name)
			}
			portOwners[port] = name
		}
	}

	// Primary service must declare all listener guest ports (v1 listeners target primary by default).
	// Skip this check for single-service apps since port conflicts are impossible.
	if len(services) > 1 {
		primarySvc := services[primary]
		primaryPorts := make(map[int]struct{}, len(primarySvc.BindPorts))
		for _, p := range primarySvc.BindPorts {
			primaryPorts[p] = struct{}{}
		}
		for _, l := range listeners {
			if _, ok := primaryPorts[l.GuestPort]; !ok {
				return fmt.Errorf("primary service '%s' must declare listener guest_port %d in bind_ports", primary, l.GuestPort)
			}
		}
	}

	// Explicit persistent volume sharing rules.
	type volumeRef struct {
		service   string
		sizeLimit string
		shared    bool
	}
	refs := make(map[string][]volumeRef)
	for svcName, svc := range services {
		if svc.Storage == nil || svc.Storage.Persistent == nil {
			continue
		}
		for volName, vol := range svc.Storage.Persistent {
			size := strings.TrimSpace(vol.SizeLimit)
			refs[volName] = append(refs[volName], volumeRef{
				service:   svcName,
				sizeLimit: size,
				shared:    vol.Shared,
			})
		}
	}

	for volName, volRefs := range refs {
		if len(volRefs) <= 1 {
			continue
		}
		wantSize := volRefs[0].sizeLimit
		for _, r := range volRefs {
			if r.sizeLimit != wantSize {
				return fmt.Errorf("persistent volume '%s' has conflicting size_limit across services", volName)
			}
			if !r.shared {
				return fmt.Errorf("persistent volume '%s' is referenced by multiple services; set shared: true for service '%s'", volName, r.service)
			}
		}
	}

	return nil
}

// validateName validates names (for instance IDs) - allows hyphens.
// For app names, use hostname.ValidateAppName() which is stricter (no hyphens).
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}

	if len(name) > 50 {
		return fmt.Errorf("name must be 50 characters or less")
	}

	if !appNameRegex.MatchString(name) {
		return fmt.Errorf("name must contain only lowercase letters, numbers, and hyphens, and must start with a letter")
	}

	// Reserved names check - use unified list from hostname package (RFC 20260130)
	for _, r := range hostname.ReservedNames {
		if name == r {
			return fmt.Errorf("name '%s' is reserved", name)
		}
	}

	return nil
}

// validateType validates app type field
func validateType(appType string) error {
	validTypes := []string{"user", "system"}
	for _, valid := range validTypes {
		if appType == valid {
			return nil
		}
	}
	return fmt.Errorf("type must be either 'user' or 'system', got '%s'", appType)
}

// validateListeners validates listener configurations.
// Workspace mode apps are allowed to have no listeners (blank environments).
// Per RFC 20260130, apps with listeners must have exactly one __primary listener.
func validateListeners(listeners []api.AppListener, mode PiccoloMode) error {
	if len(listeners) == 0 {
		// Workspace mode apps can have no listeners - they're blank environments
		// where users add ports as needed via Edit Listeners
		if mode == ModeWorkspace {
			return nil
		}
		return fmt.Errorf("listeners are required; legacy ports are no longer supported")
	}

	names := make(map[string]struct{})
	guestPorts := make(map[string]string) // keyed by "port/transport" (e.g., "53/tcp", "53/udp")
	portClaims := make(map[string]string) // keyed by "port/transport" for intra-app uniqueness
	hasPrimaryMarker := false

	for i, l := range listeners {
		// name required
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("listener[%d] name is required", i)
		}

		// RFC 20260130: __primary is a magic marker that gets replaced at install time.
		// It's exempt from normal name validation but still must be unique.
		// Also accept Primary: true for programmatically created definitions (tests, reconcile).
		isPrimary := hostname.IsPrimaryMarker(l.Name) || l.Primary
		if isPrimary {
			if hasPrimaryMarker {
				return fmt.Errorf("only one listener can be named '__primary' or have Primary=true")
			}
			hasPrimaryMarker = true

			// Per plan §D8: flow:tcp + protocol:raw is now allowed as primary —
			// the TLS mux's SNI-based routing terminates TLS for the client,
			// then proxies raw bytes to the backend. Apps with raw-TCP backends
			// (DNS-over-TLS, custom TCP protocols) can declare __primary on
			// flow:tcp+protocol:raw and get hostname routing on remote +
			// LAN host-based access. flow:udp remains the only rejected primary
			// (port-based routing only, no host-label channel).
		}

		// Validate listener name only if not __primary marker
		if !hostname.IsPrimaryMarker(l.Name) {
			var err error
			if isPrimary {
				// Primary listener name becomes app identity — must not be reserved
				err = hostname.ValidateListenerName(l.Name)
			} else {
				// Non-primary: derived hostname is <listener>-<app>, no collision with bare reserved names
				err = hostname.ValidateListenerNameFormat(l.Name)
			}
			if err != nil {
				return fmt.Errorf("listener[%d] %w", i, err)
			}
		}

		// unique name per app
		if _, ok := names[l.Name]; ok {
			return fmt.Errorf("duplicate listener name '%s'", l.Name)
		}
		names[l.Name] = struct{}{}

		// guest_port required and valid
		if l.GuestPort < 1 || l.GuestPort > 65535 {
			return fmt.Errorf("listener '%s' guest_port must be between 1 and 65535", l.Name)
		}
		// Uniqueness is per (guest_port, transport). TCP and TLS share the TCP
		// transport namespace; UDP is separate. This allows DNS-style apps to
		// declare both TCP 53 and UDP 53 as separate listeners.
		gpKey := fmt.Sprintf("%d/%s", l.GuestPort, l.Flow.TransportProtocol())
		if existing, ok := guestPorts[gpKey]; ok {
			return fmt.Errorf("guest_port %d/%s used by both '%s' and '%s'", l.GuestPort, l.Flow.TransportProtocol(), existing, l.Name)
		}
		guestPorts[gpKey] = l.Name

		if l.Flow != api.FlowTCP && l.Flow != api.FlowTLS && l.Flow != api.FlowUDP {
			return fmt.Errorf("listener '%s' flow must be 'tcp', 'tls', or 'udp'", l.Name)
		}

		// UDP is a transport protocol — HTTP and WebSocket are TCP-based and
		// make no sense over UDP.
		if l.Flow == api.FlowUDP && (l.Protocol == api.ListenerProtocolHTTP || l.Protocol == api.ListenerProtocolWebsocket) {
			return fmt.Errorf("listener '%s' flow: udp cannot be used with protocol: %s", l.Name, l.Protocol.String())
		}

		switch l.Protocol {
		case api.ListenerProtocolRaw, api.ListenerProtocolHTTP, api.ListenerProtocolWebsocket:
			// ok
		default:
			return fmt.Errorf("listener '%s' protocol '%s' not supported in v1", l.Name, l.Protocol.String())
		}

		// middleware entries: ensure names present
		for j, m := range l.Middleware {
			if strings.TrimSpace(m.Name) == "" {
				return fmt.Errorf("listener '%s' middleware[%d] name is required", l.Name, j)
			}
		}

		// RFC 20260112: Listener-level auth rules (path-based).
		if l.Auth != nil {
			if l.Flow != api.FlowTCP || (l.Protocol != api.ListenerProtocolHTTP && l.Protocol != api.ListenerProtocolWebsocket) {
				if l.Flow == api.FlowTLS || l.Protocol == api.ListenerProtocolRaw {
					return newValidationError("INVALID_AUTH_PROTOCOL", "auth block not supported on flow: tls or protocol: raw")
				}
				return newValidationError("INVALID_AUTH_FLOW", "auth block requires flow: tcp with protocol: http or websocket")
			}

			for _, rule := range l.Auth.Rules {
				ruleType := strings.TrimSpace(rule.Type)
				if ruleType == "" {
					return newValidationError("INVALID_MATCH_TYPE", "auth.rules[].type is required and must be one of: exact, prefix, pattern")
				}
				switch ruleType {
				case "exact", "prefix", "pattern":
					// ok
				default:
					return newValidationError("INVALID_MATCH_TYPE", "auth.rules[].type is required and must be one of: exact, prefix, pattern")
				}

				strategy := strings.TrimSpace(rule.Strategy)
				switch strategy {
				case "oidc_passthrough", "headers", "protected", "public":
					// ok
				default:
					return newValidationError("INVALID_STRATEGY", fmt.Sprintf("invalid strategy \"%s\", must be one of: oidc_passthrough, headers, protected, public", strategy))
				}

				rulePath := strings.TrimSpace(rule.Path)
				if rulePath == "" {
					return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": path is required", rule.Path, rule.Type))
				}
				switch ruleType {
				case "prefix":
					if !strings.HasSuffix(rulePath, "/") {
						return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": prefix paths must end with '/'", rule.Path, rule.Type))
					}
				case "pattern":
					if _, err := path.Match(rulePath, "/"); err != nil {
						return newValidationError("INVALID_PATH", fmt.Sprintf("invalid path \"%s\" for type \"%s\": %s", rule.Path, rule.Type, err.Error()))
					}
				}
			}
		}

		// RFC 20260112 §ConnectionAuth: L4 IP-based access rules. Permitted on
		// every flow including UDP (per plan §D6 / §D12). Rules must carry
		// strategy ∈ {allow, deny} and a parseable CIDR.
		if l.ConnectionAuth != nil {
			if d := strings.TrimSpace(l.ConnectionAuth.Default); d != "" && d != "allow" && d != "deny" {
				return newValidationError("INVALID_CONN_AUTH_DEFAULT", fmt.Sprintf("listener '%s' connection_auth.default must be \"allow\" or \"deny\", got %q", l.Name, d))
			}
			for j, rule := range l.ConnectionAuth.Rules {
				strategy := strings.TrimSpace(rule.Strategy)
				if strategy != "allow" && strategy != "deny" {
					return newValidationError("INVALID_CONN_AUTH_STRATEGY", fmt.Sprintf("listener '%s' connection_auth.rules[%d].strategy must be \"allow\" or \"deny\", got %q", l.Name, j, rule.Strategy))
				}
				match := strings.TrimSpace(rule.Match)
				if match == "" {
					return newValidationError("INVALID_CONN_AUTH_MATCH", fmt.Sprintf("listener '%s' connection_auth.rules[%d].match is required", l.Name, j))
				}
				if _, _, err := net.ParseCIDR(match); err != nil {
					return newValidationError("INVALID_CONN_AUTH_MATCH", fmt.Sprintf("listener '%s' connection_auth.rules[%d].match: invalid CIDR %q: %v", l.Name, j, rule.Match, err))
				}
			}
		}

		// port_claim validation
		if l.PortClaim != nil {
			pc := *l.PortClaim
			if pc < 1 || pc > 65535 {
				return fmt.Errorf("listener '%s' port_claim must be between 1 and 65535", l.Name)
			}
			// Reserved ports used by the portal and piccolod internals.
			switch pc {
			case 80, 443:
				return fmt.Errorf("listener '%s' port_claim %d is reserved for the portal", l.Name, pc)
			case 8080:
				return fmt.Errorf("listener '%s' port_claim %d is reserved for the captive portal", l.Name, pc)
			case 5353:
				return fmt.Errorf("listener '%s' port_claim %d is reserved for mDNS", l.Name, pc)
			}
			// Reject claims in the auto-allocate ranges to prevent collisions.
			if pc >= 15000 && pc <= 25000 {
				return fmt.Errorf("listener '%s' port_claim %d conflicts with host-bind range (15000-25000)", l.Name, pc)
			}
			if pc >= 35000 && pc <= 45000 {
				return fmt.Errorf("listener '%s' port_claim %d conflicts with public auto-allocate range (35000-45000)", l.Name, pc)
			}
			// Intra-app uniqueness: no two listeners can claim the same (port, transport) pair.
			claimKey := fmt.Sprintf("%d/%s", pc, l.Flow.TransportProtocol())
			if existing, ok := portClaims[claimKey]; ok {
				return fmt.Errorf("port_claim %d/%s used by both '%s' and '%s'", pc, l.Flow.TransportProtocol(), existing, l.Name)
			}
			portClaims[claimKey] = l.Name
		}
	}

	// RFC 20260130: Apps with listeners must have exactly one primary listener.
	// For raw YAML, this is the __primary marker which gets replaced at install time.
	// For programmatic definitions (tests, reconcile), this is Primary=true.
	// No auto-select for single-listener apps - all apps must explicitly designate primary.
	if !hasPrimaryMarker {
		return fmt.Errorf("apps with listeners must have exactly one listener named '__primary' or with Primary=true")
	}

	return nil
}

func usesOIDCPassthrough(listeners []api.AppListener) bool {
	for _, l := range listeners {
		if l.Auth == nil {
			continue
		}
		for _, rule := range l.Auth.Rules {
			if strings.TrimSpace(rule.Strategy) == "oidc_passthrough" {
				return true
			}
		}
	}
	return false
}

func hasOIDCClient(services map[string]api.AppService) bool {
	for _, svc := range services {
		if svc.OIDCClient != nil {
			return true
		}
	}
	return false
}

func validateOIDCRedirectURI(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if strings.TrimSpace(u.Scheme) == "" {
		return fmt.Errorf("scheme required")
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		host := strings.TrimSpace(u.Hostname())
		if host == "" {
			return fmt.Errorf("host required")
		}

		if strings.EqualFold(host, "localhost") {
			return nil
		}

		if ip := net.ParseIP(host); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				if ip4[0] == 127 && ip4[1] == 0 && ip4[2] == 0 && ip4[3] == 1 {
					return nil
				}
				return fmt.Errorf("host must be localhost or loopback")
			}
			if ip.Equal(net.IPv6loopback) {
				return nil
			}
		}
		return fmt.Errorf("host must be localhost or loopback")
	default:
		// Custom scheme redirect URIs are allowed.
		return nil
	}
}

// validateOIDCRedirectPath validates a redirect URI path segment.
// Paths must:
// - Start with "/"
// - Not be empty or whitespace-only
// - Not contain query strings or fragments
// - Contain only valid URI path characters (RFC 3986)
// Note: No normalization is performed per RFC 3986 §6.2.1 (simple string comparison).
func validateOIDCRedirectPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("redirect_uri_paths entry cannot be empty")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("redirect_uri_paths entry %q must start with /", path)
	}
	if strings.Contains(path, "?") {
		return fmt.Errorf("redirect_uri_paths entry %q must not contain query string", path)
	}
	if strings.Contains(path, "#") {
		return fmt.Errorf("redirect_uri_paths entry %q must not contain fragment", path)
	}
	// Validate path contains only valid URI characters by attempting to parse it.
	// This catches spaces, control characters, and other invalid characters.
	testURI := "http://example.com" + path
	parsed, err := url.Parse(testURI)
	if err != nil {
		return fmt.Errorf("redirect_uri_paths entry %q contains invalid characters: %v", path, err)
	}
	// Ensure the path wasn't modified during parsing (e.g., spaces encoded).
	// If the raw path differs from input, the input contained characters that needed encoding.
	if parsed.Path != path {
		return fmt.Errorf("redirect_uri_paths entry %q contains characters that require encoding (got %q after parsing)", path, parsed.Path)
	}
	return nil
}

// validateInitScript validates a service-level init script block.
func validateInitScript(svcName string, init *api.ServiceInitScript) error {
	if init == nil {
		return nil
	}
	if strings.TrimSpace(init.File) == "" {
		return fmt.Errorf("services.%s.init_script.file is required", svcName)
	}
	// Reject path traversal and absolute paths.
	if strings.Contains(init.File, "..") {
		return fmt.Errorf("services.%s.init_script.file must not contain '..'", svcName)
	}
	if path.IsAbs(init.File) {
		return fmt.Errorf("services.%s.init_script.file must be a relative path", svcName)
	}
	if len(init.File) > 256 {
		return fmt.Errorf("services.%s.init_script.file path too long (max 256)", svcName)
	}
	if !utf8.ValidString(init.File) {
		return fmt.Errorf("services.%s.init_script.file must be valid UTF-8", svcName)
	}
	// Validate shell: absolute path or bare command name. Enforce length cap
	// and restrictive character set for defense-in-depth.
	if init.Shell != "" {
		if len(init.Shell) > 128 {
			return fmt.Errorf("services.%s.init_script.shell too long (max 128)", svcName)
		}
		if !initShellRegex.MatchString(init.Shell) {
			return fmt.Errorf("services.%s.init_script.shell contains invalid characters: %s", svcName, init.Shell)
		}
		if strings.Contains(init.Shell, "/") && !path.IsAbs(init.Shell) {
			return fmt.Errorf("services.%s.init_script.shell must be an absolute path or bare command name, got '%s'", svcName, init.Shell)
		}
	}
	// Validate env keys and values.
	for k, v := range init.Env {
		if err := container.ValidateEnvKey(k); err != nil {
			return fmt.Errorf("services.%s.init_script.env key '%s': %w", svcName, k, err)
		}
		if err := container.ValidateEnvValue(v); err != nil {
			return fmt.Errorf("services.%s.init_script.env value for '%s': %w", svcName, k, err)
		}
	}
	// Validate timeout.
	if init.Timeout != "" {
		d, err := time.ParseDuration(init.Timeout)
		if err != nil {
			return fmt.Errorf("services.%s.init_script.timeout: %w", svcName, err)
		}
		if d < time.Second || d > time.Hour {
			return fmt.Errorf("services.%s.init_script.timeout must be between 1s and 3600s", svcName)
		}
	}
	// Validate ready_timeout.
	if init.ReadyTimeout != "" {
		d, err := time.ParseDuration(init.ReadyTimeout)
		if err != nil {
			return fmt.Errorf("services.%s.init_script.ready_timeout: %w", svcName, err)
		}
		if d < 0 || d > 10*time.Minute {
			return fmt.Errorf("services.%s.init_script.ready_timeout must be between 0s and 600s", svcName)
		}
	}
	return nil
}

// validateStorage validates storage configuration
func validateStorage(storage *api.AppStorage) error {
	if storage == nil {
		return nil // Storage is optional
	}

	// Validate persistent storage
	if err := validateStorageVolumes(storage.Persistent, "persistent"); err != nil {
		return err
	}

	// Validate temporary storage
	if err := validateStorageVolumes(storage.Temporary, "temporary"); err != nil {
		return err
	}

	// Prevent conflicting mountpoints inside the container.
	seen := make(map[string]string)
	for name, vol := range storage.Persistent {
		if vol.Container == "" {
			continue
		}
		if prev, ok := seen[vol.Container]; ok {
			return fmt.Errorf("storage volume 'persistent.%s' container path %s conflicts with %s", name, vol.Container, prev)
		}
		seen[vol.Container] = fmt.Sprintf("persistent.%s", name)
	}
	for name, vol := range storage.Temporary {
		if vol.Container == "" {
			continue
		}
		if prev, ok := seen[vol.Container]; ok {
			return fmt.Errorf("storage volume 'temporary.%s' container path %s conflicts with %s", name, vol.Container, prev)
		}
		seen[vol.Container] = fmt.Sprintf("temporary.%s", name)
	}

	return nil
}

// validateStorageVolumes validates a map of storage volumes
func validateStorageVolumes(volumes map[string]api.AppVolume, storageType string) error {
	if volumes == nil {
		return nil
	}

	for name, volume := range volumes {
		if name == "" {
			return fmt.Errorf("%s storage volume name cannot be empty", storageType)
		}

		if volume.Container == "" {
			return fmt.Errorf("%s storage volume '%s' must specify container path", storageType, name)
		}

		// Host paths are not user-configurable in Piccolo (all host paths live under the app's encrypted volume).
		if strings.TrimSpace(volume.Host) != "" {
			return fmt.Errorf("%s storage volume '%s' must not specify host; Piccolo manages host paths", storageType, name)
		}

		// Validate container path is absolute
		if !strings.HasPrefix(volume.Container, "/") {
			return fmt.Errorf("%s storage volume '%s' container path must be absolute", storageType, name)
		}

		// Validate size limit format if specified
		if volume.SizeLimit != "" {
			if err := validateSizeLimit(volume.SizeLimit); err != nil {
				return fmt.Errorf("%s storage volume '%s' size limit invalid: %w", storageType, name, err)
			}
		}
	}

	return nil
}

// validateResources validates the app-level resource-stewardship block.
// Authors declare classifications (priority, profile) plus a memory floor;
// runtime derives kernel knobs. No target/ceiling numbers in the manifest
// itself. See plans/resource-stewardship.md D-1, D-2, D-5, D-6.
func validateResources(resources *api.AppResources) error {
	if resources == nil {
		return nil // Resources block is optional at the app level.
	}

	// Decode-time catch-all for legacy `resources.limits` that slipped past
	// the raw-YAML shape check (e.g. alias/merge-key smuggling). The raw
	// scan is the primary rejection path; this is the belt-and-suspenders.
	if resources.LegacyLimits != nil {
		return fmt.Errorf("resources.limits is no longer supported (pre-v2 resource-stewardship schema); declare priority/memory/storage at the resources top level. Run 'piccolod catalog sync' to fetch an updated manifest.")
	}

	if err := validateResourcePriority(resources.Priority); err != nil {
		return err
	}
	if err := validateResourceMemory(resources.Memory); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if err := validateResourceStorage(resources.Storage); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	return nil
}

func validateResourcePriority(p api.ResourcePriority) error {
	switch p {
	case "", api.PriorityHigh, api.PriorityNormal, api.PriorityBackground:
		return nil
	default:
		return fmt.Errorf("priority must be one of: high, normal, background (got %q)", p)
	}
}

func validateResourceMemory(m *api.ResourceMemory) error {
	if m == nil {
		return nil
	}
	if strings.TrimSpace(m.MinRequired) == "" {
		return fmt.Errorf("min_required is required when memory block is declared")
	}
	if err := validateSizeLimit(m.MinRequired); err != nil {
		return fmt.Errorf("min_required invalid: %w", err)
	}
	switch m.Profile {
	case "", api.ProfileBounded, api.ProfileElastic:
		return nil
	default:
		return fmt.Errorf("profile must be one of: bounded, elastic (got %q)", m.Profile)
	}
}

func validateResourceStorage(s *api.ResourceStorage) error {
	if s == nil {
		return nil
	}
	if s.Max != "" {
		if err := validateSizeLimit(s.Max); err != nil {
			return fmt.Errorf("max invalid: %w", err)
		}
	}
	return nil
}

// validatePermissions validates permissions configuration
func validatePermissions(permissions *api.AppPermissions) error {
	if permissions == nil {
		return nil // Permissions are optional
	}

	// Validate network permissions
	if permissions.Network != nil {
		if err := validateNetworkPermissions(permissions.Network); err != nil {
			return err
		}
	}

	// Validate resource permissions
	if permissions.Resources != nil {
		if err := validateResourcePermissions(permissions.Resources); err != nil {
			return err
		}
	}

	return nil
}

// validateNetworkPermissions validates network permission settings
func validateNetworkPermissions(network *api.AppNetworkPermissions) error {
	validValues := []string{"allow", "deny", ""}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"internet", network.Internet},
		{"local_network", network.LocalNetwork},
		{"dns", network.DNS},
	} {
		if field.value != "" {
			found := false
			for _, valid := range validValues {
				if field.value == valid {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("network.%s must be 'allow' or 'deny', got '%s'", field.name, field.value)
			}
		}
	}

	return nil
}

// validateResourcePermissions validates resource permission settings
func validateResourcePermissions(resources *api.AppResourcePermissions) error {
	if resources.Privileged {
		return fmt.Errorf("privileged containers are not supported")
	}
	if resources.MaxProcesses < 0 {
		return fmt.Errorf("max_processes must be non-negative")
	}
	if resources.MaxOpenFiles < 0 {
		return fmt.Errorf("max_open_files must be non-negative")
	}
	return nil
}

// validateSizeLimit validates a size string against the authoritative
// parser in internal/resources.ParseSize. Single source of truth for the
// size-string grammar (integer + [B|KB|MB|GB|TB|KiB|MiB|GiB|TiB]).
// Empty input is treated as "not specified" (the field is optional).
func validateSizeLimit(limit string) error {
	if limit == "" {
		return nil
	}
	if _, err := resources.ParseSize(limit); err != nil {
		return err
	}
	return nil
}

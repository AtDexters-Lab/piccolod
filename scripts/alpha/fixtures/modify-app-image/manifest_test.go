package main

import (
	"os"
	"strings"
	"testing"

	"piccolod/internal/app"
)

// Validate the actual shared fixture template before spending VM time on it.
func TestModifyImageFixtureManifest(t *testing.T) {
	template, err := os.ReadFile("app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name := "mi0123456789ab"
	if err := app.ValidateInstanceID(name); err != nil {
		t.Fatalf("fixture address: %v", err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{"127.0.0.1:5001/fixture@" + digest, "127.0.0.1:5001/fixture:v2-alias"} {
		manifest := strings.ReplaceAll(string(template), "__MAIN_IMAGE__", ref)
		manifest = strings.ReplaceAll(manifest, "__SIDE_IMAGE__", "127.0.0.1:5001/fixture@"+digest)
		definition, err := app.ParseAppDefinition([]byte(manifest))
		if err != nil {
			t.Fatalf("parse fixture for %s: %v", ref, err)
		}
		listener := definition.Listeners[0]
		if listener.Auth == nil || len(listener.Auth.Rules) != 1 {
			t.Fatal("fixture HTTP probes need an explicit public path-auth rule")
		}
		rule := listener.Auth.Rules[0]
		if rule.Path != "/" || rule.Type != "prefix" || rule.Strategy != "public" {
			t.Fatalf("fixture auth must permit both / and /sentinel without a session: %+v", rule)
		}
		definition.Listeners[0].Name = name
		definition.Listeners[0].Primary = true
		if err := app.ValidateAppDefinition(definition); err != nil {
			t.Fatalf("validate rendered fixture for %s: %v", ref, err)
		}
	}
}

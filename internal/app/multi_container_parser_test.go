package app

import (
	"testing"

	"piccolod/internal/api"
)

func TestValidateAppDefinition_MultiContainer_ServiceMode(t *testing.T) {
	// RFC 20260130: listener name is the app identity, set Primary=true for test
	app := &api.AppDefinition{
		Type: "user",
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "web",
		Services: map[string]api.AppService{
			"web": {
				Image:     "docker.io/library/nginx:alpine",
				BindPorts: []int{8080},
				Environment: map[string]string{
					"FOO": "bar",
				},
			},
			"db": {
				Image:     "docker.io/library/postgres:16",
				After:     []string{"web"}, // ordering only
				BindPorts: []int{5432},
				Storage: &api.AppStorage{
					Persistent: map[string]api.AppVolume{
						"pgdata": {Container: "/var/lib/postgresql/data"},
					},
				},
			},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err != nil {
		t.Fatalf("expected valid multi-container app, got error: %v", err)
	}
}

func TestValidateAppDefinition_WorkspaceRequiresSingleService(t *testing.T) {
	// RFC 20260130: workspace apps without listeners use workspace_name
	app := &api.AppDefinition{
		WorkspaceName: "demo",
		Type:          "user",
		Services: map[string]api.AppService{
			"main": {Image: "alpine:latest", BindPorts: []int{}},
			"side": {Image: "alpine:latest", BindPorts: []int{}},
		},
		Extensions: map[string]interface{}{"mode": "workspace"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err == nil {
		t.Fatalf("expected error but got none")
	}
}

func TestValidateAppDefinition_MultiContainer_RejectsTopLevelContainerFields(t *testing.T) {
	// RFC 20260130: listener name is the app identity, set Primary=true for test
	app := &api.AppDefinition{
		Image: "nginx:alpine", // This should be rejected - must use services
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:alpine", BindPorts: []int{8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err == nil {
		t.Fatalf("expected error but got none")
	}
}

func TestValidateAppDefinition_MultiContainer_RejectsBindPortCollision(t *testing.T) {
	// RFC 20260130: listener name is the app identity, set Primary=true for test
	app := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {Image: "nginx:alpine", BindPorts: []int{8080}},
			"side": {Image: "alpine:latest", BindPorts: []int{8080}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err == nil {
		t.Fatalf("expected collision error but got none")
	}
}

func TestValidateAppDefinition_MultiContainer_RequiresPrimaryBindPortsForListeners(t *testing.T) {
	// This test verifies that multi-container apps require bind_ports to include listener guest_ports.
	// The check only applies when there are 2+ services (single-service apps skip this check).
	// RFC 20260130: listener name is the app identity, set Primary=true for test
	app := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		PrimaryService: "main",
		Services: map[string]api.AppService{
			"main": {Image: "nginx:alpine", BindPorts: []int{}},
			"db":   {Image: "postgres:16", BindPorts: []int{5432}},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err == nil {
		t.Fatalf("expected error but got none")
	}
}

func TestValidateAppDefinition_MultiContainer_ExplicitVolumeSharingRequired(t *testing.T) {
	// RFC 20260130: listener name is the app identity, set Primary=true for test
	app := &api.AppDefinition{
		Listeners: []api.AppListener{
			{Name: "demo", GuestPort: 8080, Flow: api.FlowTCP, Protocol: api.ListenerProtocolHTTP, Primary: true},
		},
		Services: map[string]api.AppService{
			"main": {
				Image:     "nginx:alpine",
				BindPorts: []int{8080},
				Storage: &api.AppStorage{
					Persistent: map[string]api.AppVolume{
						"cache": {Container: "/cache"},
					},
				},
			},
			"worker": {
				Image:     "alpine:latest",
				BindPorts: []int{},
				Storage: &api.AppStorage{
					Persistent: map[string]api.AppVolume{
						"cache": {Container: "/cache"},
					},
				},
			},
		},
		Extensions: map[string]interface{}{"mode": "service"},
	}

	SetDefaults(app)
	if err := ValidateAppDefinition(app); err == nil {
		t.Fatalf("expected shared volume validation error but got none")
	}
}

func TestParseAppDefinition_MultiContainer_RejectsWaitForBlock(t *testing.T) {
	manifest := `
name: demo
listeners:
  - name: web
    guest_port: 8080
services:
  main:
    image: nginx:alpine
    bind_ports: [8080]
    wait_for:
      tcp:
        host: 127.0.0.1
        port: 5432
x-piccolo:
  mode: service
`

	_, err := ParseAppDefinition([]byte(manifest))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "wait_for") {
		t.Fatalf("expected wait_for rejection, got %q", err.Error())
	}
}

func TestParseAppDefinition_MultiContainer_RejectsUnknownServiceField(t *testing.T) {
	manifest := `
name: demo
listeners:
  - name: web
    guest_port: 8080
services:
  main:
    image: nginx:alpine
    bind_ports: [8080]
    unknown_field: true
x-piccolo:
  mode: service
`

	_, err := ParseAppDefinition([]byte(manifest))
	if err == nil {
		t.Fatalf("expected error but got none")
	}
	if !containsString(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported-field rejection, got %q", err.Error())
	}
}

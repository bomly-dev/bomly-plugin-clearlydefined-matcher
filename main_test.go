package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bomly-dev/bomly-sdk"
)

func TestCoordinateFromPURL(t *testing.T) {
	pkg := &sdk.Package{Coordinates: sdk.Coordinates{PURL: "pkg:composer/acme/widget@1.2.3", Version: "1.2.3"}}
	got, ok := coordinateFromPackage(pkg)
	if !ok {
		t.Fatal("expected coordinate")
	}
	want := "composer/packagist/acme/widget/1.2.3"
	if got != want {
		t.Fatalf("coordinate = %q, want %q", got, want)
	}
}

func TestMatchFetchesLicense(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/definitions/composer/packagist/acme/widget/1.2.3" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(response{Licensed: licensed{Declared: "MIT"}})
	}))
	defer server.Close()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "plugin.json")
	if err := os.WriteFile(configPath, []byte(`{"api_base":"`+server.URL+`","cache_dir":"`+filepath.ToSlash(filepath.Join(configDir, "cache"))+`"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(sdk.EnvPluginConfigFile, configPath)

	registry := sdk.NewPackageRegistry()
	registry.Add(&sdk.Package{Coordinates: sdk.Coordinates{PURL: "pkg:composer/acme/widget@1.2.3", Name: "widget", Org: "acme", Version: "1.2.3", Ecosystem: sdk.EcosystemPHP}})
	resp, err := (&matcher{}).Match(context.Background(), &sdk.MatchRequest{Registry: registry, Graph: sdk.New()})
	if err != nil {
		t.Fatalf("Match() error = %v", err)
	}
	pkg, ok := resp.Registry.Get("pkg:composer/acme/widget@1.2.3")
	if !ok {
		t.Fatal("package missing")
	}
	if len(pkg.Licenses) != 1 || pkg.Licenses[0].SPDXExpression != "MIT" || pkg.Licenses[0].Type != sourceType {
		t.Fatalf("licenses = %#v", pkg.Licenses)
	}
}

// The declared ecosystems are what `bomly plugins list` and the marketplace
// show, so they have to match what coordinateFromGraphPackage can actually
// build a coordinate for. Conda is declared too but resolves only through the
// PURL path, so it is checked separately.
func TestSupportedEcosystemsMatchCoordinateMapping(t *testing.T) {
	descriptor, err := (&matcher{}).Descriptor(context.Background())
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}

	declared := make(map[sdk.Ecosystem]bool, len(descriptor.SupportedEcosystems))
	for _, eco := range descriptor.SupportedEcosystems {
		declared[eco] = true
	}
	if !declared[sdk.EcosystemConda] {
		t.Error("conda resolves through the PURL path and should be declared")
	}

	candidates := []sdk.Ecosystem{
		sdk.EcosystemPHP, sdk.EcosystemDPKG, sdk.EcosystemSwift, sdk.EcosystemNPM,
		sdk.EcosystemMaven, sdk.EcosystemScala, sdk.EcosystemGo, sdk.EcosystemPython,
		sdk.EcosystemRust, sdk.EcosystemRuby, sdk.EcosystemDotNet, sdk.EcosystemDart,
		sdk.EcosystemElixir, sdk.EcosystemHaskell,
	}
	for _, eco := range candidates {
		// Maven coordinates need a groupId, so give every candidate an Org and
		// let the mapping decide whether it uses one.
		pkg := &sdk.Package{Coordinates: sdk.Coordinates{
			Ecosystem: eco, Org: "com.example", Name: "example", Version: "1.0.0",
		}}
		_, mappable := coordinateFromGraphPackage(pkg)
		if mappable && !declared[eco] {
			t.Errorf("coordinateFromGraphPackage handles %q but it is not declared", eco)
		}
		if !mappable && declared[eco] && eco != sdk.EcosystemConda {
			t.Errorf("%q is declared but coordinateFromGraphPackage cannot map it", eco)
		}
	}
}

// A Maven artifact without a groupId cannot be addressed in ClearlyDefined, so
// it should be skipped rather than sent as a malformed coordinate.
func TestMavenWithoutGroupIDIsSkipped(t *testing.T) {
	pkg := &sdk.Package{Coordinates: sdk.Coordinates{
		Ecosystem: sdk.EcosystemMaven, Name: "orphan", Version: "1.0.0",
	}}
	if coord, ok := coordinateFromGraphPackage(pkg); ok {
		t.Errorf("expected no coordinate without a groupId, got %q", coord)
	}
}

// The descriptor's ConfigSchema is generated from the config struct, so a
// renamed or retyped field would silently change the advertised schema. Pin
// the property names and types the plugin actually decodes.
func TestDescriptorConfigSchema(t *testing.T) {
	descriptor, err := (&matcher{}).Descriptor(context.Background())
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if len(descriptor.ConfigSchema) == 0 {
		t.Fatal("descriptor ConfigSchema is empty")
	}

	var schema struct {
		Type                 string                    `json:"type"`
		Properties           map[string]map[string]any `json:"properties"`
		AdditionalProperties *bool                     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(descriptor.ConfigSchema, &schema); err != nil {
		t.Fatalf("ConfigSchema is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("expected object schema, got %q", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("expected additionalProperties to be false")
	}

	expected := map[string]string{
		"api_base":      "string",
		"cache_dir":     "string",
		"cache_ttl":     "string",
		"disable_cache": "boolean",
	}
	if len(schema.Properties) != len(expected) {
		t.Fatalf("expected %d properties, got %#v", len(expected), schema.Properties)
	}
	for name, wantType := range expected {
		property, ok := schema.Properties[name]
		if !ok {
			t.Errorf("missing property %q", name)
			continue
		}
		if property["type"] != wantType {
			t.Errorf("property %q has type %#v, want %q", name, property["type"], wantType)
		}
	}
}

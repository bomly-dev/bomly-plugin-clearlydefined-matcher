package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bomly-dev/bomly-cli/sdk"
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

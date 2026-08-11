package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	sdk "github.com/bomly-dev/bomly-sdk"
	"github.com/bomly-dev/bomly-sdk/conformance"
	"go.uber.org/zap"
)

// testHost is a minimal HostContext for unit tests.
type testHost struct {
	config json.RawMessage
}

func (h testHost) Logger() *zap.Logger                 { return zap.NewNop() }
func (h testHost) HTTPClient() *sdk.HTTPClientProvider { return nil }
func (h testHost) Runtime() sdk.RuntimeInfo {
	return sdk.RuntimeInfo{Execution: sdk.ExecutionEmbedded}
}

func (h testHost) DecodeConfig(v any) error {
	payload := h.config
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	return json.Unmarshal(payload, v)
}

func newMatcher(t *testing.T, config json.RawMessage) sdk.Matcher {
	t.Helper()
	matcher, err := Module().Matcher.New(context.Background(), testHost{config: config})
	if err != nil {
		t.Fatalf("construct matcher: %v", err)
	}
	return matcher
}

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

	cfg := `{"api_base":"` + server.URL + `","cache_dir":"` + filepath.ToSlash(filepath.Join(t.TempDir(), "cache")) + `"}`
	matcher := newMatcher(t, json.RawMessage(cfg))

	registry := sdk.NewPackageRegistry()
	registry.Add(&sdk.Package{Coordinates: sdk.Coordinates{PURL: "pkg:composer/acme/widget@1.2.3", Name: "widget", Org: "acme", Version: "1.2.3", Ecosystem: sdk.EcosystemPHP}})
	resp, err := matcher.Match(context.Background(), sdk.MatchRequest{Registry: registry, Graph: sdk.New()})
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
	descriptor := Module().Matcher.Descriptor

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

// A config block that does not decode must surface through Ready as the
// not-ready reason instead of failing construction, so the host can report
// it (the ReadyResponse.Reason contract from the legacy serving style).
func TestInvalidConfigSurfacesThroughReady(t *testing.T) {
	matcher := newMatcher(t, json.RawMessage(`{"api_base":42}`))
	if err := matcher.Ready(context.Background(), sdk.MatchRequest{}); err == nil {
		t.Fatal("expected Ready to report the invalid configuration")
	}
	if _, err := matcher.Match(context.Background(), sdk.MatchRequest{Registry: sdk.NewPackageRegistry()}); err == nil {
		t.Fatal("expected Match to refuse to run with an invalid configuration")
	}
}

// The descriptor's ConfigSchema is generated from the config struct, so a
// renamed or retyped field would silently change the advertised schema. Pin
// the property names and types the plugin actually decodes.
func TestDescriptorConfigSchema(t *testing.T) {
	descriptor := Module().Matcher.Descriptor
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

// TestConformance runs the SDK conformance suite against the module,
// including the bomly-plugin.json identity cross-check.
func TestConformance(t *testing.T) {
	conformance.Test(t, conformance.Config{
		Module:       Module(),
		ManifestPath: filepath.Join("..", "bomly-plugin.json"),
		SampleConfig: json.RawMessage(`{"api_base":"https://api.clearlydefined.io","cache_ttl":"12h","disable_cache":true}`),
	})
}

// TestProbeBinary builds the real plugin binary and probes it over the
// managed HashiCorp gRPC transport, asserting the served descriptor equals
// the in-process one.
func TestProbeBinary(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available; skipping managed-transport probe")
	}
	binaryPath := filepath.Join(t.TempDir(), "bomly-plugin-clearlydefined-matcher")
	build := exec.Command(goBinary, "build", "-o", binaryPath, "./cmd/bomly-plugin-clearlydefined-matcher")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin binary: %v\n%s", err, output)
	}
	conformance.ProbeBinary(t, binaryPath, conformance.WithModule(Module()))
}

// Package plugin implements the ClearlyDefined license matcher: a Bomly
// MATCHER that fills missing package license data from the curated
// ClearlyDefined definitions service.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bomly-dev/bomly-sdk"
)

// Name is the plugin's identity. It MUST equal the "id" field in
// bomly-plugin.json — Bomly refuses to load a plugin whose manifest id and
// runtime descriptor name disagree.
const Name = "clearlydefined-license-matcher"

const (
	// licenseSource names this component as the supplier of a license claim.
	// It is written to PackageLicense.Source -- "who says so" -- and never to
	// PackageLicense.Type, which is the SDK's closed two-member provenance
	// vocabulary ("declared" / "concluded"). This matcher used to write its
	// own name into Type; the SDK gate now drops anything outside that
	// vocabulary, which silently erased the claim's supplier.
	licenseSource   = "external-clearlydefined"
	defaultAPIBase  = "https://api.clearlydefined.io"
	defaultCacheTTL = 24 * time.Hour
)

// Matcher is the component. Configuration is decoded once at construction
// through the HostContext; a decode failure is remembered and surfaced
// through Ready so the host can report the reason instead of hard-failing.
type Matcher struct {
	config    config
	configErr error
	http      *sdk.HTTPClientProvider
}

type config struct {
	APIBase      string `json:"api_base"`
	CacheDir     string `json:"cache_dir"`
	CacheTTL     string `json:"cache_ttl"`
	DisableCache bool   `json:"disable_cache"`
}

// descriptor is the matcher's static registration data.
func descriptor() sdk.MatcherDescriptor {
	return sdk.MatcherDescriptor{
		Name:         Name,
		DisplayName:  "ClearlyDefined License Matcher",
		Aliases:      []string{"clearlydefined"},
		Tags:         []string{"license-enrichment", "http", "cache"},
		ConfigSchema: sdk.MustConfigSchemaFor(config{}),
		Capabilities: []string{sdk.CapabilityPackageUpdates},
		// Mirrors the coordinate mappings below. Anything outside this set has
		// no ClearlyDefined coordinate to build, so it is skipped without a
		// request — leaving this empty would read as "every ecosystem".
		//
		// The matcher only fills packages that have no licence yet, so
		// overlapping with Bomly's built-in licence matchers is additive:
		// ClearlyDefined is curated and often has an answer where deps.dev,
		// which covers seven ecosystems best-effort, does not.
		//
		// Still missing: go (go/golang), and the git and sourcearchive types,
		// which are commit-addressed rather than version-addressed. See #7.
		SupportedEcosystems: []sdk.Ecosystem{
			sdk.EcosystemNPM,    // npm/npmjs
			sdk.EcosystemMaven,  // maven/mavencentral
			sdk.EcosystemScala,  // maven/mavencentral
			sdk.EcosystemPython, // pypi/pypi
			sdk.EcosystemDotNet, // nuget/nuget
			sdk.EcosystemRuby,   // gem/rubygems
			sdk.EcosystemRust,   // crate/cratesio
			sdk.EcosystemPHP,    // composer/packagist
			sdk.EcosystemDPKG,   // deb/debian
			sdk.EcosystemSwift,  // pod/cocoapods
			sdk.EcosystemConda,  // conda/{anaconda-main,anaconda-r,conda-forge}
		},
	}
}

// Descriptor identifies the matcher to Bomly.
func (m *Matcher) Descriptor() sdk.MatcherDescriptor { return descriptor() }

// Ready reports whether the matcher can run; an invalid configuration is
// reported as the not-ready reason rather than a construction failure.
func (m *Matcher) Ready(context.Context, sdk.MatchRequest) error {
	if m.configErr != nil {
		return fmt.Errorf("invalid clearlydefined matcher configuration: %w", m.configErr)
	}
	return nil
}

// Applicable reports whether the request carries a graph and a package
// registry to enrich.
func (m *Matcher) Applicable(_ context.Context, req sdk.MatchRequest) (bool, error) {
	return req.Graph != nil && req.Registry != nil, nil
}

// Match fills missing license data for registry packages from ClearlyDefined.
//
// Two response shapes exist. Legacy hosts get the request registry back,
// enriched in place (the protocol v1 baseline). When the request sets
// AcceptPackageUpdates, the registry is left untouched and the result carries
// PackageUpdates instead: one delta per enriched package holding only the
// PURL, the resolved licenses, and Matched. The host merges deltas by PURL;
// MergeFrom fills Licenses only when the package has none — exactly the
// packages this matcher enriches — and ORs Matched in, so applying the deltas
// reproduces the in-place enrichment.
func (m *Matcher) Match(ctx context.Context, req sdk.MatchRequest) (sdk.MatchResult, error) {
	useDeltas := req.AcceptPackageUpdates
	if req.Registry == nil {
		return matchResponse(nil, nil, useDeltas, 0, 0, 0), nil
	}
	if m.configErr != nil {
		return sdk.MatchResult{}, fmt.Errorf("invalid clearlydefined matcher configuration: %w", m.configErr)
	}
	cfg := m.config
	client, err := m.httpClient()
	if err != nil {
		return sdk.MatchResult{}, err
	}
	cache := newFileCache(cfg.CacheDir, cfg.CacheTTL, cfg.DisableCache)
	matchedPackages := 0
	licenses := 0
	unmatchedPackages := 0
	var updates []*sdk.Package
	record := func(pkg *sdk.Package, values []string) {
		count := 0
		if useDeltas {
			if built := buildLicenses(pkg, values); len(built) > 0 {
				updates = append(updates, &sdk.Package{
					Coordinates: sdk.Coordinates{PURL: pkg.PURL},
					Matched:     true,
					Licenses:    built,
				})
				count = len(built)
			}
		} else {
			count = applyLicenses(pkg, values)
		}
		if count > 0 {
			matchedPackages++
			licenses += count
		} else {
			unmatchedPackages++
		}
	}
	for _, pkg := range req.Registry.All() {
		if pkg == nil || len(pkg.Licenses) > 0 {
			continue
		}
		coordinate, ok := coordinateFromPackage(pkg)
		if !ok {
			unmatchedPackages++
			continue
		}
		if values, ok := cache.get(coordinate); ok {
			record(pkg, values)
			continue
		}
		values, err := fetchDefinition(ctx, client, cfg.APIBase, coordinate)
		if err != nil {
			return matchResponse(req.Registry, updates, useDeltas, matchedPackages, unmatchedPackages, licenses), err
		}
		_ = cache.set(coordinate, values)
		record(pkg, values)
	}
	return matchResponse(req.Registry, updates, useDeltas, matchedPackages, unmatchedPackages, licenses), nil
}

func matchResponse(registry *sdk.PackageRegistry, updates []*sdk.Package, useDeltas bool, matchedPackages, unmatchedPackages, licenses int) sdk.MatchResult {
	if useDeltas {
		registry = nil
	} else {
		updates = nil
	}
	return sdk.MatchResult{
		Registry:       registry,
		PackageUpdates: updates,
		MatcherStats: sdk.MatcherStats{
			Name:              Name,
			DisplayName:       "ClearlyDefined License Matcher",
			MatchedPackages:   matchedPackages,
			UnmatchedPackages: unmatchedPackages,
			Licenses:          licenses,
		},
	}
}

func loadConfig(host sdk.HostContext) (config, error) {
	cfg := config{
		APIBase:  defaultAPIBase,
		CacheDir: defaultCacheDir(),
		CacheTTL: defaultCacheTTL.String(),
	}
	if err := host.DecodeConfig(&cfg); err != nil {
		return config{}, err
	}
	if strings.TrimSpace(cfg.APIBase) == "" {
		cfg.APIBase = defaultAPIBase
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		cfg.CacheDir = defaultCacheDir()
	}
	if strings.TrimSpace(cfg.CacheTTL) == "" {
		cfg.CacheTTL = defaultCacheTTL.String()
	}
	return cfg, nil
}

func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".bomly-cache", "licenses", "clearlydefined")
	}
	return filepath.Join(home, ".bomly", "cache", "licenses", "clearlydefined")
}

func (m *Matcher) httpClient() (*http.Client, error) {
	provider := m.http
	if provider == nil {
		created, err := sdk.NewHTTPClientProvider(sdk.HTTPClientConfig{})
		if err != nil {
			return nil, err
		}
		provider = created
	}
	return provider.Client(20 * time.Second), nil
}

// Module packages the matcher for both execution modes: Bomly can embed it
// in-process or serve it as a managed plugin subprocess (see
// cmd/bomly-plugin-clearlydefined-matcher).
func Module() sdk.Module {
	return sdk.Module{
		Kind: sdk.PluginKindMatcher,
		Matcher: &sdk.MatcherModule{
			Descriptor: descriptor(),
			New: func(_ context.Context, host sdk.HostContext) (sdk.Matcher, error) {
				matcher := &Matcher{http: host.HTTPClient()}
				matcher.config, matcher.configErr = loadConfig(host)
				return matcher, nil
			},
		},
	}
}

func fetchDefinition(ctx context.Context, client *http.Client, apiBase, coordinate string) ([]string, error) {
	endpoint := strings.TrimRight(apiBase, "/") + "/definitions/" + coordinate
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("clearlydefined: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clearlydefined: execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("clearlydefined: request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var definition response
	if err := json.NewDecoder(resp.Body).Decode(&definition); err != nil {
		return nil, fmt.Errorf("clearlydefined: decode response: %w", err)
	}
	return definition.licenseValues(), nil
}

// buildLicenses converts raw ClearlyDefined license values into the package
// license records this matcher contributes. It returns nil when pkg already
// has licenses or the values normalize to nothing.
func buildLicenses(pkg *sdk.Package, values []string) []sdk.PackageLicense {
	values = normalizeLicenseSet(values)
	if pkg == nil || len(pkg.Licenses) > 0 || len(values) == 0 {
		return nil
	}
	licenses := make([]sdk.PackageLicense, 0, len(values))
	for _, value := range values {
		licenses = append(licenses, sdk.PackageLicense{Value: value, SPDXExpression: value, Source: licenseSource})
	}
	return licenses
}

func applyLicenses(pkg *sdk.Package, values []string) int {
	licenses := buildLicenses(pkg, values)
	if len(licenses) == 0 {
		return 0
	}
	pkg.Licenses = licenses
	pkg.Matched = true
	return len(licenses)
}

func normalizeLicenseSet(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == "NOASSERTION" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func coordinateFromPackage(pkg *sdk.Package) (string, bool) {
	if pkg == nil || strings.TrimSpace(pkg.Version) == "" {
		return "", false
	}
	if parsed, ok := parsePURL(strings.TrimSpace(pkg.PURL)); ok {
		if coordinate, ok := coordinateFromParsedPURL(parsed); ok {
			return coordinate, true
		}
	}
	return coordinateFromGraphPackage(pkg)
}

func coordinateFromGraphPackage(pkg *sdk.Package) (string, bool) {
	name := strings.TrimSpace(pkg.Name)
	org := strings.TrimSpace(pkg.Org)
	version := strings.TrimSpace(pkg.Version)
	if name == "" || version == "" {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(string(pkg.Ecosystem))) {
	case "php":
		namespace := firstNonEmpty(org, "-")
		return "composer/packagist/" + escapeSegment(namespace) + "/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "dpkg":
		return "deb/debian/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "swift":
		return "pod/cocoapods/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "npm":
		return "npm/npmjs/" + escapeSegment(firstNonEmpty(org, "-")) + "/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "maven", "scala":
		if org == "" {
			return "", false
		}
		return "maven/mavencentral/" + escapeSegment(org) + "/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "rust":
		return "crate/cratesio/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "ruby":
		return "gem/rubygems/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "python":
		return "pypi/pypi/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	case "dotnet":
		return "nuget/nuget/-/" + escapeSegment(name) + "/" + escapeSegment(version), true
	default:
		return "", false
	}
}

type parsedPURL struct {
	Type       string
	Namespace  string
	Name       string
	Version    string
	Qualifiers map[string]string
}

func parsePURL(value string) (parsedPURL, bool) {
	if !strings.HasPrefix(value, "pkg:") {
		return parsedPURL{}, false
	}
	trimmed := strings.TrimPrefix(value, "pkg:")
	trimmed = strings.SplitN(trimmed, "#", 2)[0]
	typeAndPath := trimmed
	qualifierText := ""
	if base, qualifiers, ok := strings.Cut(trimmed, "?"); ok {
		typeAndPath = base
		qualifierText = qualifiers
	}
	typeAndPath, version, _ := strings.Cut(typeAndPath, "@")
	typeValue, rawPath, ok := strings.Cut(typeAndPath, "/")
	if !ok {
		return parsedPURL{}, false
	}
	decodedPath, err := url.PathUnescape(rawPath)
	if err != nil {
		decodedPath = rawPath
	}
	parts := strings.Split(decodedPath, "/")
	if len(parts) == 0 {
		return parsedPURL{}, false
	}
	qualifiers := make(map[string]string)
	for _, part := range strings.Split(qualifierText, "&") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		key, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		decodedVal, err := url.QueryUnescape(val)
		if err != nil {
			decodedVal = val
		}
		qualifiers[strings.ToLower(strings.TrimSpace(key))] = decodedVal
	}
	name := parts[len(parts)-1]
	namespace := ""
	if len(parts) > 1 {
		namespace = strings.Join(parts[:len(parts)-1], "/")
	}
	return parsedPURL{
		Type:       strings.ToLower(strings.TrimSpace(typeValue)),
		Namespace:  strings.TrimSpace(namespace),
		Name:       strings.TrimSpace(name),
		Version:    strings.TrimSpace(version),
		Qualifiers: qualifiers,
	}, name != ""
}

func coordinateFromParsedPURL(p parsedPURL) (string, bool) {
	if p.Version == "" {
		return "", false
	}
	switch p.Type {
	case "composer":
		namespace := firstNonEmpty(p.Namespace, "-")
		return "composer/packagist/" + escapeSegment(namespace) + "/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "deb":
		return "deb/debian/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "cocoapods":
		return "pod/cocoapods/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "npm":
		// Scoped packages keep the scope as the namespace: npm/npmjs/@babel/core.
		namespace := firstNonEmpty(p.Namespace, "-")
		return "npm/npmjs/" + escapeSegment(namespace) + "/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "maven":
		// ClearlyDefined requires the groupId, so an artifact without one
		// cannot be addressed.
		if strings.TrimSpace(p.Namespace) == "" {
			return "", false
		}
		return "maven/mavencentral/" + escapeSegment(p.Namespace) + "/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "cargo":
		return "crate/cratesio/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "gem":
		return "gem/rubygems/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "pypi":
		return "pypi/pypi/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "nuget":
		return "nuget/nuget/-/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	case "conda":
		channel := strings.TrimSpace(p.Qualifiers["channel"])
		subdir := strings.TrimSpace(p.Qualifiers["subdir"])
		provider := condaProvider(channel)
		if provider == "" || subdir == "" {
			return "", false
		}
		return "conda/" + escapeSegment(provider) + "/" + escapeSegment(subdir) + "/" + escapeSegment(p.Name) + "/" + escapeSegment(p.Version), true
	default:
		return "", false
	}
}

func condaProvider(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "main":
		return "anaconda-main"
	case "r":
		return "anaconda-r"
	case "conda-forge":
		return "conda-forge"
	default:
		return ""
	}
}

func escapeSegment(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return url.PathEscape(strings.TrimSpace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type response struct {
	Licensed licensed `json:"licensed"`
}

type licensed struct {
	Declared string `json:"declared"`
	Facets   struct {
		Core struct {
			Discovered struct {
				Expressions []string `json:"expressions"`
			} `json:"discovered"`
		} `json:"core"`
	} `json:"facets"`
}

func (r response) licenseValues() []string {
	if strings.TrimSpace(r.Licensed.Declared) != "" {
		return []string{strings.TrimSpace(r.Licensed.Declared)}
	}
	return r.Licensed.Facets.Core.Discovered.Expressions
}

type fileCache struct {
	dir      string
	ttl      time.Duration
	disabled bool
}

func newFileCache(dir, ttlText string, disabled bool) fileCache {
	ttl, err := time.ParseDuration(ttlText)
	if err != nil || ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return fileCache{dir: dir, ttl: ttl, disabled: disabled}
}

func (c fileCache) get(key string) ([]string, bool) {
	if c.disabled || c.dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	var entry struct {
		CreatedAt time.Time `json:"created_at"`
		Values    []string  `json:"values"`
	}
	if err := json.Unmarshal(data, &entry); err != nil || time.Since(entry.CreatedAt) > c.ttl {
		return nil, false
	}
	return entry.Values, true
}

func (c fileCache) set(key string, values []string) error {
	if c.disabled || c.dir == "" {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	entry := struct {
		CreatedAt time.Time `json:"created_at"`
		Values    []string  `json:"values"`
	}{CreatedAt: time.Now().UTC(), Values: values}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path(key), data, 0o644)
}

func (c fileCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".json")
}

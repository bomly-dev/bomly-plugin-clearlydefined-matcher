package main

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

	"github.com/bomly-dev/bomly-cli/sdk"
)

const (
	matcherName     = "clearlydefined-license-matcher"
	sourceType      = "external-clearlydefined"
	defaultAPIBase  = "https://api.clearlydefined.io"
	defaultCacheTTL = 24 * time.Hour
)

type matcher struct{}

type config struct {
	APIBase      string `json:"api_base"`
	CacheDir     string `json:"cache_dir"`
	CacheTTL     string `json:"cache_ttl"`
	DisableCache bool   `json:"disable_cache"`
}

func (m *matcher) Descriptor(context.Context) (*sdk.MatcherDescriptor, error) {
	return &sdk.MatcherDescriptor{
		Name:        matcherName,
		DisplayName: "ClearlyDefined License Matcher",
		Aliases:     []string{"clearlydefined"},
		Tags:        []string{"license-enrichment", "http", "cache"},
	}, nil
}

func (m *matcher) Ready(context.Context, *sdk.MatchRequest) (*sdk.ReadyResponse, error) {
	if _, err := loadConfig(); err != nil {
		return &sdk.ReadyResponse{Reason: "invalid clearlydefined matcher configuration: " + err.Error()}, nil
	}
	return &sdk.ReadyResponse{Ready: true}, nil
}

func (m *matcher) Applicable(_ context.Context, req *sdk.MatchRequest) (*sdk.ApplicableResponse, error) {
	if req.Graph == nil || req.Registry == nil {
		return &sdk.ApplicableResponse{Applicable: false}, nil
	}
	return &sdk.ApplicableResponse{Applicable: true}, nil
}

func (m *matcher) Match(ctx context.Context, req *sdk.MatchRequest) (*sdk.MatchResponse, error) {
	if req.Registry == nil {
		return matchResponse(req.Registry, 0, 0, 0), nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	client, err := httpClient()
	if err != nil {
		return nil, err
	}
	cache := newFileCache(cfg.CacheDir, cfg.CacheTTL, cfg.DisableCache)
	matchedPackages := 0
	licenses := 0
	unmatchedPackages := 0
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
			if count := applyLicenses(pkg, values); count > 0 {
				matchedPackages++
				licenses += count
			} else {
				unmatchedPackages++
			}
			continue
		}
		values, err := fetchDefinition(ctx, client, cfg.APIBase, coordinate)
		if err != nil {
			return matchResponse(req.Registry, matchedPackages, unmatchedPackages, licenses), err
		}
		_ = cache.set(coordinate, values)
		if count := applyLicenses(pkg, values); count > 0 {
			matchedPackages++
			licenses += count
		} else {
			unmatchedPackages++
		}
	}
	return matchResponse(req.Registry, matchedPackages, unmatchedPackages, licenses), nil
}

func matchResponse(registry *sdk.PackageRegistry, matchedPackages, unmatchedPackages, licenses int) *sdk.MatchResponse {
	return &sdk.MatchResponse{
		Registry: registry,
		MatcherStats: sdk.MatcherStats{
			Name:              matcherName,
			DisplayName:       "ClearlyDefined License Matcher",
			MatchedPackages:   matchedPackages,
			UnmatchedPackages: unmatchedPackages,
			Licenses:          licenses,
		},
	}
}

func loadConfig() (config, error) {
	cfg := config{
		APIBase:  defaultAPIBase,
		CacheDir: defaultCacheDir(),
		CacheTTL: defaultCacheTTL.String(),
	}
	if err := sdk.DecodePluginConfigFromEnv(&cfg); err != nil {
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

func httpClient() (*http.Client, error) {
	provider, err := sdk.NewHTTPClientProviderFromEnv()
	if err != nil {
		return nil, err
	}
	return provider.Client(20 * time.Second), nil
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

func applyLicenses(pkg *sdk.Package, values []string) int {
	values = normalizeLicenseSet(values)
	if pkg == nil || len(pkg.Licenses) > 0 || len(values) == 0 {
		return 0
	}
	licenses := make([]sdk.PackageLicense, 0, len(values))
	for _, value := range values {
		licenses = append(licenses, sdk.PackageLicense{Value: value, SPDXExpression: value, Type: sourceType})
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

func main() {
	sdk.ServeMatcher(&matcher{})
}

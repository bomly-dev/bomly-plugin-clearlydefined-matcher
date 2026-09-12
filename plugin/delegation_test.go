package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/bomly-dev/bomly-sdk"
)

// TestLicenseValuesAreClassifiedNotAsserted pins the classification contract.
// ClearlyDefined answers with whatever its curators and scanners recorded, and
// "Apache 2.0", "OTHER" and "SEE LICENSE IN LICENSE" arrive through the same
// field as "MIT". Value always carries the trimmed input; SPDXExpression is
// filled only when the value validates as SPDX, and holds the canonical
// spelling when it does.
func TestLicenseValuesAreClassifiedNotAsserted(t *testing.T) {
	cases := []struct {
		name  string
		value string
		spdx  string
	}{
		{"identifier", "MIT", "MIT"},
		{"identifier lowercase folds to canonical case", "apache-2.0", "Apache-2.0"},
		{"deprecated identifier folds to replacement", "GPL-2.0", "GPL-2.0-only"},
		{"deprecated plus form folds to or-later", "GPL-3.0+", "GPL-3.0-or-later"},
		{"expression", "MIT OR Apache-2.0", "MIT OR Apache-2.0"},
		{"parenthesised expression canonicalises", "(MIT OR CC0-1.0)", "MIT OR CC0-1.0"},
		{"expression with exception", "Apache-2.0 WITH LLVM-exception", "Apache-2.0 WITH LLVM-exception"},
		{"licence reference", "LicenseRef-scancode-unknown", "LicenseRef-scancode-unknown"},
		{"free text name", "Apache 2.0", ""},
		{"free text prose", "SEE LICENSE IN LICENSE", ""},
		{"clearlydefined sentinel OTHER", "OTHER", ""},
		{"clearlydefined sentinel NONE", "NONE", ""},
		{"npm sentinel UNLICENSED", "UNLICENSED", ""},
		{"family name without a version", "BSD", ""},
		{"scancode wildcard", "MIT*", ""},
		{"syntactically invalid expression", "MIT OR", ""},
		{"unregistered word", "Commercial", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			licenses := buildLicenses(&sdk.Package{}, []string{tc.value})
			if len(licenses) != 1 {
				t.Fatalf("buildLicenses(%q) = %#v, want one licence", tc.value, licenses)
			}
			got := licenses[0]
			if got.Value != strings.TrimSpace(tc.value) {
				t.Errorf("Value = %q, want %q", got.Value, strings.TrimSpace(tc.value))
			}
			if got.SPDXExpression != tc.spdx {
				t.Errorf("SPDXExpression = %q, want %q", got.SPDXExpression, tc.spdx)
			}
			if got.Source != licenseSource {
				t.Errorf("Source = %q, want %q", got.Source, licenseSource)
			}
			// ClearlyDefined serves licensed.declared and a discovered
			// expression through one field, so which provenance applies is
			// not knowable here. Leaving Type empty is the decision, not an
			// omission.
			if got.Type != "" {
				t.Errorf("Type = %q, want empty", got.Type)
			}
		})
	}
}

// TestLicenseSetIsFilteredDeduplicatedAndOrdered pins the pre-pass this
// package still owns: NOASSERTION is ClearlyDefined's "nothing was asserted"
// sentinel and must never become a licence value, and the emitted order is
// sorted so a package's licences do not shuffle between runs.
func TestLicenseSetIsFilteredDeduplicatedAndOrdered(t *testing.T) {
	licenses := buildLicenses(&sdk.Package{}, []string{"MIT", "NOASSERTION", "Apache-2.0", "  MIT  ", ""})
	var values []string
	for _, license := range licenses {
		values = append(values, license.Value)
	}
	want := []string{"Apache-2.0", "MIT"}
	if len(values) != len(want) {
		t.Fatalf("values = %#v, want %#v", values, want)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("values = %#v, want %#v", values, want)
		}
	}
	if got := buildLicenses(&sdk.Package{}, []string{"NOASSERTION"}); len(got) != 0 {
		t.Fatalf("NOASSERTION alone produced %#v, want no licences", got)
	}
}

// TestCoordinateFromPackageURL pins the coordinates delegated package-URL
// parsing produces, including the three cases where the hand-rolled parser
// this replaced answered differently.
func TestCoordinateFromPackageURL(t *testing.T) {
	cases := []struct {
		name string
		purl string
		want string
	}{
		{"composer", "pkg:composer/acme/widget@1.2.3", "composer/packagist/acme/widget/1.2.3"},
		{"composer case folds to packagist's canonical lowercase", "pkg:composer/ACME/Widget@1.2.3", "composer/packagist/acme/widget/1.2.3"},
		{"npm unscoped", "pkg:npm/lodash@4.17.21", "npm/npmjs/-/lodash/4.17.21"},
		{"npm scope encoded", "pkg:npm/%40babel/core@7.24.0", "npm/npmjs/@babel/core/7.24.0"},
		{"npm scope literal", "pkg:npm/@babel/core@7.24.0", "npm/npmjs/@babel/core/7.24.0"},
		// A producer that records the scope without its '@' used to address
		// "npm/npmjs/babel/core", which ClearlyDefined answers for nothing.
		{"npm scope missing its at sign is repaired", "pkg:npm/babel/core@7.24.0", "npm/npmjs/@babel/core/7.24.0"},
		{"maven", "pkg:maven/org.apache.commons/commons-lang3@3.14.0", "maven/mavencentral/org.apache.commons/commons-lang3/3.14.0"},
		{"maven qualifiers are ignored", "pkg:maven/org.apache.commons/commons-lang3@3.14.0?type=jar", "maven/mavencentral/org.apache.commons/commons-lang3/3.14.0"},
		{"pypi name takes its normalised form", "pkg:pypi/zope_interface@5.4.0", "pypi/pypi/-/zope-interface/5.4.0"},
		{"pypi name case folds", "pkg:pypi/Django@4.2.0", "pypi/pypi/-/django/4.2.0"},
		{"cargo", "pkg:cargo/serde@1.0.197", "crate/cratesio/-/serde/1.0.197"},
		{"gem", "pkg:gem/rails@7.1.3", "gem/rubygems/-/rails/7.1.3"},
		{"nuget", "pkg:nuget/Newtonsoft.Json@13.0.3", "nuget/nuget/-/Newtonsoft.Json/13.0.3"},
		{"deb", "pkg:deb/debian/curl@7.88.1", "deb/debian/-/curl/7.88.1"},
		{"cocoapods", "pkg:cocoapods/AFNetworking@4.0.1", "pod/cocoapods/-/AFNetworking/4.0.1"},
		{"conda", "pkg:conda/numpy@1.26.0?channel=main&subdir=linux-64", "conda/anaconda-main/linux-64/numpy/1.26.0"},
		{"conda forge", "pkg:conda/numpy@1.26.0?subdir=linux-64&channel=conda-forge", "conda/conda-forge/linux-64/numpy/1.26.0"},
		{"conda without a subdir has no coordinate", "pkg:conda/numpy@1.26.0?channel=main", ""},
		{"maven without a group has no coordinate", "pkg:maven/commons-lang3@3.14.0", ""},
		{"unmapped type has no coordinate", "pkg:golang/github.com/pkg/errors@v0.9.1", ""},
		{"version-less package URL has no coordinate", "pkg:npm/lodash", ""},
		// The parser refuses these outright; before delegation the first two
		// produced "npm/npmjs/-/-/4.17.21" and "npm/npmjs/@types%2Fnode".
		{"blank name is refused", "pkg:npm/ @4.17.21", ""},
		{"component carrying a separator is refused", "pkg:npm/%40types%2Fnode@20.0.0", ""},
		{"non package URL is refused", "not-a-purl", ""},
		{"empty is refused", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkg := &sdk.Package{Coordinates: sdk.Coordinates{PURL: tc.purl, Version: "probe"}}
			got, ok := coordinateFromPackage(pkg)
			if tc.want == "" {
				if ok {
					t.Fatalf("coordinateFromPackage(%q) = %q, want no coordinate", tc.purl, got)
				}
				return
			}
			if !ok {
				t.Fatalf("coordinateFromPackage(%q) reported no coordinate, want %q", tc.purl, tc.want)
			}
			if got != tc.want {
				t.Fatalf("coordinateFromPackage(%q) = %q, want %q", tc.purl, got, tc.want)
			}
		})
	}
}

// TestRefusedPackageURLFallsBackToGraphCoordinates pins that refusing a
// package URL costs nothing in production: the package's own org, name and
// version answer instead. This is the case the hand-rolled parser got right by
// accident and the delegated one gets right by construction.
func TestRefusedPackageURLFallsBackToGraphCoordinates(t *testing.T) {
	cases := []struct {
		name string
		pkg  *sdk.Package
		want string
	}{
		{
			name: "component carrying a separator",
			pkg: &sdk.Package{Coordinates: sdk.Coordinates{
				PURL: "pkg:npm/%40types%2Fnode@20.0.0", Name: "node", Org: "@types", Version: "20.0.0", Ecosystem: sdk.EcosystemNPM,
			}},
			want: "npm/npmjs/@types/node/20.0.0",
		},
		{
			name: "blank name",
			pkg: &sdk.Package{Coordinates: sdk.Coordinates{
				PURL: "pkg:npm/ @4.17.21", Name: "lodash", Version: "4.17.21", Ecosystem: sdk.EcosystemNPM,
			}},
			want: "npm/npmjs/-/lodash/4.17.21",
		},
		{
			name: "not a package URL at all",
			pkg: &sdk.Package{Coordinates: sdk.Coordinates{
				PURL: "not-a-purl", Name: "widget", Org: "acme", Version: "1.2.3", Ecosystem: sdk.EcosystemPHP,
			}},
			want: "composer/packagist/acme/widget/1.2.3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := coordinateFromPackage(tc.pkg)
			if !ok || got != tc.want {
				t.Fatalf("coordinateFromPackage() = %q (ok=%v), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestSPDXExpressionIsNeverAssignedHere is the guard. The defect this package
// carried was one assignment -- SPDXExpression: value -- that asserted a
// validity nothing had checked, and it is one line away from coming back on
// any new licence-producing path. Classification belongs to matcherkit, which
// validates through spdxkit (ADR-0035), so no source file in this package may
// write the field itself.
func TestSPDXExpressionIsNeverAssignedHere(t *testing.T) {
	for _, file := range packageSourceFiles(t) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			kv, ok := node.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == "SPDXExpression" {
				t.Errorf("%s: assigns SPDXExpression directly; classify through matcherkit instead", fset.Position(kv.Pos()))
			}
			return true
		})
	}
}

// TestPackageURLsAreNotParsedHere is the second half of the same guard:
// package-URL parsing belongs to purlkit (ADR-0038), and a hand-rolled parser
// reappears by someone reaching for the "pkg:" prefix.
func TestPackageURLsAreNotParsedHere(t *testing.T) {
	for _, file := range packageSourceFiles(t) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), `"pkg:"`) {
			t.Errorf("%s: takes apart a package URL by hand; parse through purlkit instead", file)
		}
	}
}

// packageSourceFiles lists this package's non-test Go files.
func packageSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list source files: %v", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry, "_test.go") {
			continue
		}
		files = append(files, entry)
	}
	if len(files) == 0 {
		t.Fatal("no source files found")
	}
	return files
}

# ClearlyDefined License Matcher Plugin

External Bomly matcher plugin for [ClearlyDefined](https://clearlydefined.io) license metadata. This plugin carries the matcher ID `clearlydefined-license-matcher` and the short selector alias `clearlydefined`.

## Build and test

```bash
go test ./...
go build -o bin/bomly-plugin-clearlydefined-matcher ./cmd/bomly-plugin-clearlydefined-matcher
```

## Install for local development

```bash
bomly plugin install ./bin/bomly-plugin-clearlydefined-matcher --dev
bomly plugin enable clearlydefined-license-matcher
bomly scan --enrich --matchers +clearlydefined
```

## Install from an archive

```bash
bomly plugin install ./dist/bomly-plugin-clearlydefined-matcher_linux_amd64.tar.gz
bomly plugin enable clearlydefined-license-matcher
```

Direct URL installs require a checksum unless you explicitly opt out:

```bash
bomly plugin install https://example.internal/bomly-plugin-clearlydefined-matcher_linux_amd64.tar.gz \
  --checksum sha256:<digest>
```

## Install from a private GitHub Release

```bash
export BOMLY_GITHUB_TOKEN=<token-with-release-access>
bomly plugin install github:bomly-dev/bomly-plugin-clearlydefined-matcher@v0.1.0
bomly plugin enable clearlydefined-license-matcher
```

`GITHUB_TOKEN`, `GH_TOKEN`, and `GITHUB_AUTH_TOKEN` are also accepted by Bomly for private release metadata and asset downloads.

## Configuration

Configure the plugin in Bomly's plugin config map:

```yaml
plugins:
  clearlydefined-license-matcher:
    api_base: https://api.clearlydefined.io
    cache_dir: ~/.bomly/cache/licenses/clearlydefined
    cache_ttl: 24h
    disable_cache: false
```

The plugin honors Bomly's proxy environment passed to external plugins.

## What the plugin writes

The plugin only fills packages that have no license yet. For each such package
it writes one license record per value ClearlyDefined reports:

- `value` is the raw ClearlyDefined value, trimmed and nothing else.
- `spdxExpression` is filled **only when the value really is SPDX**, and holds
  the canonical spelling: `apache-2.0` becomes `Apache-2.0`, the deprecated
  `GPL-2.0` becomes `GPL-2.0-only`, and `(MIT OR CC0-1.0)` becomes
  `MIT OR CC0-1.0`. ClearlyDefined also reports free text — `OTHER`,
  `Public Domain`, `Apache 2.0`, `SEE LICENSE IN LICENSE` — and those keep
  their `value` with `spdxExpression` left empty rather than claiming to be an
  expression they are not.
- `source` names this plugin, so you can see which component supplied a claim.
- `type` (`declared` / `concluded`) is left unset on purpose: ClearlyDefined
  serves a declared license and a discovered-expression fallback through the
  same field, so the plugin cannot tell the two apart.

`NOASSERTION` means "nothing was asserted" and is dropped rather than recorded
as a license.

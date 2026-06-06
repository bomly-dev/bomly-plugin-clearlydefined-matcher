# ClearlyDefined License Matcher Plugin

External Bomly matcher plugin for [ClearlyDefined](https://clearlydefined.io) license metadata. This plugin carries the matcher ID `clearlydefined-license-matcher` and the short selector alias `clearlydefined`.

## Build and test

```bash
go test ./...
go build -o bin/bomly-plugin-clearlydefined-matcher .
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

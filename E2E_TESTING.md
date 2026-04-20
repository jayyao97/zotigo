# E2E Configuration Guide

This document is for real-provider E2E tests only.
Runtime CLI config still uses `~/.zotigo/config.yaml`.

## Quick Start

1. Copy the example file:
   ```bash
   cp zotigo.e2e.example.yaml zotigo.e2e.yaml
   ```

2. Edit `zotigo.e2e.yaml` with your API keys and models.

3. Run E2E tests:
   ```bash
   go test -tags=e2e ./core/providers -run TestE2E_ProviderSmoke -v
   ```

## Example

```yaml
default_profile: gpt-4o

profiles:
  gpt-4o:
    provider: openai
    model: gpt-4o
    api_key: "sk-or-v1-..."
    base_url: "https://openrouter.ai/api/v1"
    safety:
      classifier:
        enabled: true
        review_threshold: medium
        profile: gpt-5.4-reasoning
        timeout_ms: 3000
        allow_auto_execute_on_allow: false

  gpt-5.4-reasoning:
    provider: openai
    model: gpt-5.4
    api_key: "sk-or-v1-..."
    base_url: "https://openrouter.ai/api/v1"
```

## File Resolution Order

The E2E helper resolves config files in this order:

1. `zotigo.e2e.yaml` (preferred)
2. `e2e.config.json` (legacy fallback)
3. `config.json` (legacy fallback)

## Security

- `zotigo.e2e.yaml` should contain test credentials only.
- Never commit real API keys.

## Troubleshooting

### "No API key configured"

Make sure the selected `provider` section has a non-empty `api_key`.

### "E2E config file not found"

Create the file from the example:

```bash
cp zotigo.e2e.example.yaml zotigo.e2e.yaml
```

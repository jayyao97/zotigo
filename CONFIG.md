# E2E Configuration Guide

This document is for real-provider E2E tests only.
Runtime CLI config still uses `~/.zotigo/config.yaml`.

## Quick Start

1. Copy the example file:
   ```bash
   cp e2e.config.example.json e2e.config.json
   ```

2. Edit `e2e.config.json` with your API keys and models.

3. Run E2E tests:
   ```bash
   go test -tags=e2e ./core/providers -run TestE2E_ProviderSmoke -v
   ```

## Example (OpenRouter)

```json
{
  "provider": "openai",
  "streaming": true,
  "user_id": "local_test",
  "openai": {
    "api_key": "sk-or-v1-...",
    "base_url": "https://openrouter.ai/api/v1",
    "model": "gpt-5.2-codex"
  },
  "anthropic": {
    "api_key": "sk-or-v1-...",
    "base_url": "https://openrouter.ai/api/v1",
    "model": "anthropic/claude-haiku-4.5"
  },
  "gemini": {
    "api_key": "sk-or-v1-...",
    "base_url": "https://openrouter.ai/api/v1",
    "model": "google/gemini-3-flash-preview"
  }
}
```

## File Resolution Order

`LoadE2EConfig()` resolves config files in this order:

1. `e2e.config.json` (preferred)
2. `config.json` (legacy fallback)

## Security

- `e2e.config.json` is ignored by git.
- Never commit real API keys.

## Troubleshooting

### "No API key configured"

Make sure the selected `provider` section has a non-empty `api_key`.

### "E2E config file not found"

Create the file from the example:

```bash
cp e2e.config.example.json e2e.config.json
```

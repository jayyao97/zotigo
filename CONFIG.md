# Configuration Guide

## Quick Start

1. Copy the example configuration:
   ```bash
   cp config.example.json config.json
   ```

2. Edit `config.json` with your API key:
   ```json
   {
     "anthropic": {
       "api_key": "your-actual-anthropic-api-key-here",
       "base_url": "https://api.anthropic.com/v1/messages",
       "model": "claude-3-5-sonnet-20241022"
     }
   }
   ```

3. Run with Claude provider:
   ```bash
   ./zotigo --provider claude
   ```

## Configuration Options

### For LiteLLM Proxy

If you're using LiteLLM to forward requests:

```json
{
  "anthropic": {
    "api_key": "your-actual-anthropic-api-key-here",
    "base_url": "http://localhost:4000/v1/chat/completions",
    "model": "claude-3-5-sonnet-20241022"
  }
}
```

### Available Models

- `claude-3-5-sonnet-20241022` (recommended)
- `claude-3-opus-20240229`
- `claude-3-sonnet-20240229`
- `claude-3-haiku-20240307`

## Priority Order

Configuration values are resolved in this order:

1. **Command line flags** (highest priority)
2. **config.json file**
3. **Environment variables** (fallback)
4. **Default values** (lowest priority)

## Security

- `config.json` is automatically ignored by git
- Never commit your API keys to version control
- Keep your config.json file secure and private

## Troubleshooting

### "config.json not found"
Create the config file using the example:
```bash
cp config.example.json config.json
```

### "API key is required"
Make sure your `config.json` contains a valid API key:
```json
{
  "anthropic": {
    "api_key": "sk-ant-api03-..."
  }
}
```

### Invalid API key error
Verify your API key is correct in the Anthropic Console.
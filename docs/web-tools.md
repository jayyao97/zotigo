# Web search backends

`web_search` supports Tavily and DeepSeek independently of the model profile.
Configure the backend in `~/.zotigo/config.yaml` or the project's `zotigo.yaml`:

```yaml
tools:
  web:
    search_provider: deepseek
    # Optional: otherwise reads DEEPSEEK_API_KEY from the daemon environment.
    # deepseek_api_key: your-key
    timeout_sec: 15
```

Use `search_provider: tavily` with `tavily_api_key` or `TAVILY_API_KEY`
to select Tavily. An omitted provider preserves existing behavior: use Tavily
when its key is available, otherwise leave `web_search` unregistered. Merely
setting `DEEPSEEK_API_KEY` does not change the default backend. An explicit
selection with no key, or an unknown provider, fails tool initialization.
Changing this configuration requires recreating the agent/worker; existing
workers retain their tool configuration. The model profile's API key is not
implicitly reused.

DeepSeek uses [the standalone Search API](https://api-docs.deepseek.com/zh-cn/guides/search/).
The tool accepts `query` (1–128 Unicode characters, including operators) and
`max_results` (default 5, capped at 20). Use `site:`, `-site:`, `lang:`,
`after:YYYY-MM-DD` and `before:YYYY-MM-DD` in the query. Tavily's `topic` and
`time_range` options are not advertised for DeepSeek; unsupported options are
rejected instead of silently ignored.

Results use `summary`, falling back to `snippet`. Full page content is not
requested. `web_fetch` continues to fetch URLs directly; DeepSeek Fetch is not
used. HTTP failures are returned as tool errors without automatic retries,
consistent with the existing Tavily backend.

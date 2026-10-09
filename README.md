# bob

bob (Backend Of Backend) is the in-cluster app server for [zengine](https://github.com/zetaoss/zengine).
zengine stays thin: features it needs from inside the cluster live in bob. bob serves what it can
directly and forwards the rest (isolated execution, storage) to upstream services. It has no Ingress;
only zengine calls it.

| Route | Handled by |
| --- | --- |
| `/healthz` | bob |
| `/aigate/` | bob: LLM routing with fallback across providers (moved from [aigate](https://github.com/zetaoss/aigate)) |
| `/<name>/` | forwarded to the upstream configured under `proxies.<name>` |

Forwarding strips the route prefix and keeps the rest of the path and the query:
`/search/search?q=x` with `search: http://search` goes to `http://search/search?q=x`.
Responses are flushed as they arrive, so streaming upstreams pass through.
Unknown routes return `404`; an unreachable upstream returns `502`.

## Configuration

See [`config.yaml.example`](config.yaml.example).

- `server.port` defaults to `8080`. `server.logLevel` is `debug`, `info` (default) or `error`; `debug` prints upstream provider requests and responses.
- `aigate` is enabled when `aigate.models` is non-empty.
  - `models` use `{provider}/{model}` and split on the first `/`. Their order is the fallback priority.
  - Providers: `gemini`, `ollama` (endpoints are fixed in code).
  - Each provider's model list is loaded once at startup for `/aigate/v1/models?all`. `validateModelsOnStartup` (default `true`) checks every configured model against it.
  - `fallback.rounds` (default `2`) is how many passes over the model chain are tried; `fallback.perAttemptTimeout` (default `30s`) is the per-attempt HTTP timeout.
  - `model` may be omitted (configured order), a provider name (that provider's models), or a configured model (tried first, then the rest).
- `proxies` maps a route name (lowercase letters, digits, `-`; not `healthz` or `aigate`) to an `http(s)` base URL.

- Unknown keys are rejected, so a misspelled key fails at startup instead of being ignored.
- `aigate.providers.*.apiKey` and `proxies` values may reference environment variables as `${NAME}`
  (an unset variable is a startup error). Keep secrets in the environment and the file in version control or a ConfigMap.

Logs are JSON lines on stderr (`log/slog`). Every request except `/healthz` gets an access log with
method, path, status and duration. API keys are printed as `[redacted]` in the startup config log.

## aigate API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/aigate/healthz` | health |
| `GET` | `/aigate/v1/models` | enabled models |
| `GET` | `/aigate/v1/models?all` | models available from providers, with `enabled` |
| `GET` | `/aigate/v1/models/{provider}` | enabled models of one provider (`?all` as above) |
| `POST` | `/aigate/v1/chat/completions` | OpenAI-style chat completion; the response adds `provider_meta` (`requested_model`, `attempted_models`) |

```sh
curl -X POST http://localhost:8080/aigate/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-2.5-flash","messages":[{"role":"user","content":"Hello"}]}'
```

## Development

```sh
cp config.yaml.example config.yaml   # config.yaml and .env are git-ignored
echo 'GEMINI_API_KEY=...' >> .env    # variables referenced as ${NAME} in config.yaml
make run                             # loads .env, then go run ./cmd/bob -config config.yaml
make test                            # go vet + go test
make curl-models                     # more targets: make help
```

Releases: pushing a `v*` tag builds `ghcr.io/zetaoss/bob:<tag>` and `:latest`.

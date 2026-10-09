# bob

bob (Backend Of Backend) is the in-cluster app server for [zengine](https://github.com/zetaoss/zengine).
zengine stays thin: features it needs from inside the cluster live in bob. bob serves what it can
directly and forwards the rest (isolated execution, storage) to upstream services. It has no Ingress;
only zengine calls it.

| Route | Handled by |
| --- | --- |
| `/healthz` | bob |
| `/aigate/` | bob: LLM routing with fallback across providers (moved from [aigate](https://github.com/zetaoss/aigate)) |
| `/search/` | bob: result counts across search engines (moved from [queryhub](https://github.com/zetaoss/queryhub)) |
| `/k8s/` | bob: node, pod, PVC and zeta-defender metrics from Prometheus |
| `/<name>/` | forwarded to the upstream configured under `proxies.<name>` |

Forwarding strips the route prefix and keeps the rest of the path and the query:
`/runbox/lang` with `runbox: http://runbox` goes to `http://runbox/lang`.
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
- `search` holds search API credentials. Each engine is used only when its credentials are set
  (`daum_blog`: Kakao key; `naver_blog`, `naver_news`: Naver client ID and secret; `google_search`: Google API key and CX).
  The route is enabled when at least one engine is.
- `k8s` is enabled when `k8s.prometheus` is set; `nodepool`, `namespace` and `pvc` are then required.
- `proxies` maps a route name (lowercase letters, digits, `-`; not `healthz`, `aigate`, `k8s` or `search`) to an `http(s)` base URL.

- Unknown keys are rejected, so a misspelled key fails at startup instead of being ignored.
- `aigate.providers.*.apiKey`, `search.*` and `proxies` values may reference environment variables as `${NAME}`
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

## search API

`GET /search/search?q=<term>&q=<term>[&q=<term>&q=<term>]` takes 2 to 4 queries and returns the result
count of each query on each engine. Engines are queried in parallel; if any call fails, the response is
`500` with the failing engine in `error`.

```json
{"status":"ok","result":{"engines":["daum_blog","google_search","naver_blog","naver_news"],"values":[[576,10700,1899,0],[840,293000000,1409,18]]}}
```

`values[i][j]` is the count for query `i` on engine `j`. Google wraps the query in double quotes (exact match).

## k8s API

`GET /k8s/metrics[?time=<RFC3339>]` returns resource usage at `time` (default: now). CPU is in
cores, memory and storage in bytes.

```json
{"status":"ok","result":{"nodepool":"pool2","namespace":"prod3",
 "nodes":[{"name":"...","cpu_usage":0.88,"cpu_allocatable":1.93,"memory_usage":8576364544,"memory_allocatable":13918404608}],
 "pods":[{"name":"...","namespace":"prod3","cpu_usage":0.12,"memory_usage":524288000}],
 "pvcs":[{"name":"db","namespace":"prod3","usage":43832832000,"capacity":52723159040,"usage_percent":83.1}],
 "defender":{"fighting_ratio":0,"max_level":0}}}
```

Nodes are those whose name contains the node pool; pods are those in the namespace. Node, pod and
zeta-defender queries are best effort (a failed query leaves zeros); a missing PVC usage or capacity is
a `502`. `defender` is the share of the last hour spent in Under Attack mode and the highest level
reached (from `zeta_defender_fighting_seconds_total` and `zeta_defender_level`).

## Development

```sh
cp config.yaml.example config.yaml   # config.yaml and .env are git-ignored
echo 'GEMINI_API_KEY=...' >> .env    # variables referenced as ${NAME} in config.yaml
make run                             # loads .env, then go run ./cmd/bob -config config.yaml
make test                            # go vet + go test
make curl-models                     # more targets: make help
```

Releases: pushing a `v*` tag builds `ghcr.io/zetaoss/bob:<tag>` and `:latest`.

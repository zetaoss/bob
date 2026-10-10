# bob

bob (Backend Of Backend) is the in-cluster app server for [zengine](https://github.com/zetaoss/zengine).
zengine stays thin: features it needs from inside the cluster live in bob. bob serves what it can
directly and forwards the rest to upstream services. It has no Ingress;
only zengine calls it.

| Route | Handled by |
| --- | --- |
| `/healthz` | bob |
| `/aigate/` | bob: LLM routing with fallback across providers (moved from [aigate](https://github.com/zetaoss/aigate)) |
| `/search/` | bob: result counts across search engines (moved from [queryhub](https://github.com/zetaoss/queryhub)) |
| `/metrics/` | bob: named PromQL queries from the config, run against Prometheus |
| `/cloudflare/` | bob: Cloudflare zone analytics per hour or day |
| `/ga/`, `/gsc/` | bob: Google Analytics 4 and Search Console reports per hour or day |
| `/runbox/` | bob: runs code in throwaway containers on a Docker daemon (moved from [runbox](https://github.com/zetaoss/runbox)) |
| `/<name>/` | forwarded to the upstream configured under `proxies.<name>` |

Forwarding strips the route prefix and keeps the rest of the path and the query:
`/shellbox-score/score` with `shellbox-score: http://shellbox-score` goes to `http://shellbox-score/score`.
Responses are flushed as they arrive, so streaming upstreams pass through.
Unknown routes return `404`; an unreachable upstream returns `502`.

## Configuration

See [`config.example.yaml`](config.example.yaml).

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
- `metrics.queries` maps a name (lowercase letters, digits, `_`) to a PromQL instant query run against
  `metrics.prometheus`. The route is enabled when at least one query is set.
- `cloudflare` is enabled when `apiToken` (Zone Analytics read) and `zoneID` are set.
- `google.serviceAccount` is a service account key (JSON) with read access to the GA4 property
  (`gaPropertyID`, enables `/ga/`) and the Search Console site (`gscSiteURL`, enables `/gsc/`).
  The GA property's time zone is read from GA's responses.
- `runbox.dockerHost` enables `/runbox/`: `tcp://host:port` with TLS client authentication
  (`caCert`, `clientCert`, `clientKey` as PEM), or `unix:///var/run/docker.sock` for local development.
- `proxies` maps a route name (lowercase letters, digits, `-`; not a built-in route) to an `http(s)` base URL.

- Unknown keys are rejected, so a misspelled key fails at startup instead of being ignored.
- `aigate.providers.*.apiKey`, `search.*`, `cloudflare.*`, `google.*`, `runbox.*` and `proxies` values may reference environment variables as `${NAME}`
  (an unset variable is a startup error). Keep secrets in the environment and the file in version control or a ConfigMap.

Logs are JSON lines on stderr (`log/slog`). Every request except `/healthz` gets an access log with
method, path, status and duration. API keys and runbox certificates are printed as `[redacted]` in the startup config log.

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

## metrics API

`GET /metrics/?name=<name>&name=<name>...[&time=<RFC3339>]` runs the named queries at `time`
(default: now); without `name` it runs every configured query, and `GET /metrics/<name>` runs one.
An unknown name is a `404` listing the unknown names, so callers can ask for exactly what they need
and fail when the config does not provide it. Queries run in parallel; if any fails, the response is `502`.

```json
{"status":"ok","time":"2026-10-09T07:00:00Z","result":{
 "node_cpu_usage":[{"labels":{"node":"node-1"},"value":0.88}],
 "pod_count":[{"labels":{},"value":10}]}}
```

Each metric is a list of samples (one per series of an instant vector, or one for a scalar). Samples
whose value is NaN or infinite are dropped, so an empty list means "no data". Write queries so the
caller can use the samples directly: `sum by (node) (...)` for per-item values that callers can also
sum, an aggregation for a single value.

## cloudflare API

`GET /cloudflare/analytics?interval=hour&since=<RFC3339>&until=<RFC3339>` and
`GET /cloudflare/analytics?interval=day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD>` (`until` exclusive) return
the zone's HTTP request analytics per timeslot. Hourly ranges are queried in 24-hour windows.

```json
{"status":"ok","result":[{"timeslot":"2026-10-09T07:00:00Z","metrics":{
 "uniq_uniques":"1234","sum_requests":"2.345678e+06","sum_countryMap":"[{\"bytes\":1,\"key\":\"KR\",\"requests\":2,\"threats\":0}]", ...}}]}
```

Metric values are text: numbers as Go's `%v` of the decoded JSON number, maps (browser, content
type, TLS, country, IP class, status, threat pathing) as JSON. Names: `uniq_uniques`, `sum_requests`,
`sum_pageViews`, `sum_bytes`, `sum_cachedBytes`, `sum_cachedRequests`, `sum_encryptedBytes`,
`sum_encryptedRequests`, `sum_threats`, and `sum_<name>Map` for the maps.

## ga and gsc API

`GET /ga/report?interval=hour|day&since=<RFC3339>&until=<RFC3339>` returns the hours starting in
`[since, until)` (timeslot RFC3339 UTC), or the property-local dates overlapping it (timeslot
`YYYY-MM-DD`). Dates (`since=YYYY-MM-DD&until=YYYY-MM-DD`, both inclusive, property-local) are also
accepted. `GET /gsc/query?interval=hour|day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD>` (both inclusive)
returns RFC3339 UTC hours or Pacific dates.

```json
{"status":"ok","result":[{"timeslot":"2026-10-09T07:00:00Z","sessions":5,"screen_page_views":9,"active_users":3}]}
{"status":"ok","result":[{"timeslot":"2026-10-09T07:00:00Z","clicks":12,"impressions":340,"ctr":3.5294,"position":7.1235}]}
```

GA dates and hours are converted with the property's time zone from GA's response metadata;
Search Console hours are Pacific time. GSC `ctr` is a percentage;
`ctr` and `position` are rounded to 4 decimals. Access tokens are reused until shortly before they expire.

## runbox API

`POST /runbox/lang` runs files of one language; `POST /runbox/notebook` runs code cells in Jupyter
(`python`, `r`) and returns each cell's outputs.

```sh
curl -X POST http://localhost:8080/runbox/lang -d '{"lang":"python","files":[{"body":"print(1)"}]}'
{"logs":["11"],"mem":1636,"time":325}
curl -X POST http://localhost:8080/runbox/notebook -d '{"lang":"python","sources":["1+1"]}'
{"outputsList":[[{"output_type":"execute_result","data":{"text/plain":["2"]},"metadata":{},"execution_count":1}]],"cpu":...,"mem":...,"time":...,"timedout":false}
```

- Languages: `bash`, `c`, `cpp`, `csharp`, `go`, `java`, `kotlin`, `latex`/`tex`, `lua`, `mysql`, `perl`,
  `php`, `powershell`, `python`, `r`, `ruby`, `sqlite3`. Images are `ghcr.io/zetaoss/runcontainers/<lang>:<version>`
  at the [runcontainers](https://github.com/zetaoss/runcontainers) release in `runcontainersVersion`
  ([`internal/runbox/images.go`](internal/runbox/images.go)). Set it to a newer release and release bob to run
  newer images. Notebooks use `<lang>-notebook` (`python-notebook`, `r-notebook`) of the same release.
  Images are pulled on first use. `bob -config config.yaml pull-images` pulls every runbox image the Docker host
  does not have yet and exits; run it in an init container so the first run after a release does not wait for a
  pull. It exits non-zero if any image is not pulled, so bob does not start until the Docker host has them all.
- `files[].name` defaults to `runbox.<ext>`; files with the same name are joined. `main` is the index of the
  file php and r may rewrite (php adds `<?php` and the autoloader; r draws plots to PNG).
- Each log is the stream (`1` stdout, `2` stderr) followed by the line. Up to 2 PNG files (10 for LaTeX) of at
  most 100 KiB from the working directory are returned base64 in `images`. `cpu` is CPU microseconds, `mem` KiB,
  `time` milliseconds. A run past its timeout (10 s; go, latex, mysql 30 s; kotlin 40 s; notebooks 60 s)
  returns what it printed with `timedout: true`.
- An unknown language, no files or no sources is `400`; a Docker failure is `500`. Errors are `{"error": ...}`.
- Each run gets a fresh container (label `bob.runbox`, 100 processes) that is force-removed afterwards.
  Before each run, `bob.runbox` containers older than 5 minutes are removed; other containers on the daemon
  are left alone.

## Development

```sh
cp config.example.yaml config.yaml   # config.yaml and .env are git-ignored
echo 'GEMINI_API_KEY=...' >> .env    # variables referenced as ${NAME} in config.yaml
make run                             # loads .env, then go run ./cmd/bob -config config.yaml
make test                            # go vet + go test
make curl-models                     # more targets: make help
```

Releases: pushing a `v*` tag builds `ghcr.io/zetaoss/bob:<tag>` and `:latest`.

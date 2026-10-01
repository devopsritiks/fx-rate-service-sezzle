# Prompts and responses

This take-home was built in six phases, each driven by a prompt to
Claude (Sonnet 5) describing the goal, constraints, and (where relevant)
explicit design asks. Below is the full text of every phase's prompt, in
order, followed by a short summary of what was built in response.

## Phase 1: Foundation

**Prompt:**

> im doing a take home assesment for principal SRE role at Sezzle, its a fintech / buy now pay later company. i work as devops/SRE so i know infra, k8s, prometheus, grafana, ci/cd and reliability stuff pretty well but i dont know Go at all. so you will write the code and i will drive the design part. explain your choices in simple SRE language as we go becuase i will get questioned on these in live interview
>
> what i want to build:
> a "FX Rate Service" in Go. it sits infront of the free Frankfurter exchange rate api (https://api.frankfurter.app, no api key needed). idea is something like a BNPL checkout would use it to show prices in USD/CAD and other currencies
>
> endpoints:
> - GET /v1/rates/{base}?symbols=USD,CAD  -> latest rates for base currency
> - GET /v1/convert?from=USD&to=CAD&amount=100.00  -> convert amount
> - GET /health  -> liveness
> - GET /ready   -> readiness
> - GET /metrics -> prometheus metrics
>
> full project we will build in phases over multiple chats. final thing should have timeouts, retries with backoff and jitter, circuit breaker, inbound rate limiting, caching with serve stale when vendor is down, request coalescing, prometheus metrics, structured logs with request id, async audit logging to postgres with connection pool metrics, graceful shutdown and docker compose stack with prometheus and grafana
>
> for now only do the foundation part:
>
> 1. suggest a clean go project structure (cmd/, internal/ etc) which can scale for all the above stuff, and tell me shortly what each folder is for
> 2. setup go module, a config package that read settings from env vars with sane defaults (port, vendor url, vendor timeout, log level). use go's standard log/slog for json logging
> 3. http server with the endpoints above working end to end with frankfurter. proper error handling atleast basic level:
>    - validate currency codes (3 uppercase letters) and amount
>    - same json error format everywhere with correct http status codes
>    - dont leak raw vendor errors or internals to the caller
> 4. this is fintech so dont use float64 for money. use some proper decimal lib like shopspring/decimal and round the converted amounts correctly
> 5. graceful shutdown on SIGTERM/SIGINT, and set server read/write/idle timeouts
> 6. /metrics endpoint using standard prometheus go client for now, custom metrics we will add later
> 7. a Makefile with run, build, test, lint targets and a small README on how to run it with some curl examples
>
> keep it production quality but dont over engineer this step. leave clear places where resilience, caching and db parts will plug in later. at the end tell me exact commands to run and test it locally (im on mac/linux terminal) and list what we should do in next phase

**Summary:** Scaffolded the project (`cmd/fxservice`, `internal/config`,
`internal/httpapi`, `internal/fxvendor/frankfurter`, `internal/money`),
env-driven config, `log/slog` JSON logging, all five endpoints working
against the real Frankfurter API, uniform JSON error envelope,
`shopspring/decimal` throughout (no `float64` for money), graceful
shutdown with configured server timeouts, stdlib Prometheus `/metrics`,
a Makefile, and a README with working curl examples.

## Phase 2: Resilience

**Prompt:**

> foundation looks good, everything working on my side too. now lets do phase 2, the resilience part around the frankfurter client. keep explaining in SRE terms like before
>
> what i want:
>
> 1. timeouts at two levels. one per attempt timeout and one overall budget for the whole call including retries. also respect the incoming request context, so if client disconnects or cancels we stop calling vendor right away
>
> 2. retries with exponential backoff and full jitter. only retry on things that make sense to retry like timeouts, connection errors, 5xx and 429. dont retry on 4xx like bad currency, that will just waste calls. if vendor sends Retry-After header respect it. max attempts, base delay and max delay should come from env config
>
> 3. circuit breaker around the vendor (sony/gobreaker is fine or whatever you think is better, tell me why). it should open after some failure ratio, go half-open after a cooldown and let few test requests through. log every state change with old and new state. when breaker is open fail fast and return a clear 503 to caller with a proper error code, dont even try the vendor
>
> 4. a bulkhead, basically limit how many concurrent calls we make to frankfurter at the same time so a slow vendor cant eat all our goroutines and connections. also tune the http.Client transport (max idle conns, idle timeout etc) and cap the vendor response body size so a huge or bad response cant blow our memory
>
> 5. inbound rate limiting per client ip using token bucket (golang.org/x/time/rate). return 429 with Retry-After header and our standard json error. limits from env. clean up old limiter entries so the map dont grow forever. dont rate limit /health, /ready and /metrics
>
> 6. /ready behaviour. i dont want readiness to fail just because vendor is down, because then k8s will pull ALL pods out of the service at same time and we turn a vendor outage into our full outage. once caching comes in next phase we can still serve stale data. so keep /ready returning 200 but show breaker state and dependency status in the response body. explain this tradeoff properly, i want to talk about it in the interview
>
> 7. order of the wrapping matters, so explain where retry sits vs breaker vs bulkhead and why you picked that order
>
> 8. tests for this. use httptest fake server to simulate vendor timeouts, 500s, 429 with Retry-After and slow responses, and prove retry, breaker open/half-open/close and rate limit all actually work. also add a simple way for me to demo the breaker opening locally, like pointing the vendor url to a dead port
>
> dont add custom prometheus metrics yet, we will do all metrics in observability phase. but keep clean hooks (like breaker state change callback, retry attempt hook) so metrics plug in easy later. update the .env.example and README with new settings
>
> at the end give me exact commands to run tests and curl commands to see retry, breaker and rate limit happening live, and tell me whats next

**Summary:** Added `internal/fxvendor/resilience` (circuit breaker via
`sony/gobreaker/v2`, bulkhead via a buffered-channel semaphore, retry
loop with full-jitter backoff and `Retry-After` honoring) wrapping the
vendor client in the explained order — breaker → bulkhead → retry →
attempt. Added `internal/ratelimit` (per-IP token bucket with a
background-sweep eviction loop). Updated `/ready` to always return 200
while reporting breaker state in the body, with the Kubernetes-fleet-
outage reasoning written up for the interview. Tuned the vendor
`http.Transport` and capped response body size. Wrote `httptest`-based
tests for retry/breaker/rate-limit behavior, and documented a
dead-port breaker demo. Updated `.env.example` and README.

## Phase 3: Caching

**Prompt:**

> phase 2 is done and working. project is in /Users/rsharma41/SPPs, continue there. now phase 3, caching. keep the SRE style explanations
>
> what i want:
>
> 1. in-memory cache in its own package (internal/cache) behind a interface, so later we could swap it with redis without touching handlers. tell me the tradeoff of in-memory vs redis when we run multiple pods, like each pod has its own cache and different hit ratio
>
> 2. cache key should be normalized, like USD with symbols CAD,EUR and EUR,CAD should hit same entry. for /v1/convert dont cache every amount seperately, just cache the rates and do the decimal math locally. that way one rates entry serves all convert requests
>
> 3. two TTLs. a fresh TTL and a max stale age, both from env. frankfurter rates only update once per working day so tell me what defaults make sense. within fresh TTL serve from cache. after fresh TTL try the vendor, and if vendor fails or breaker is open, serve the stale entry instead of failing. this is the main point of this phase
>
> 4. this is fintech so we cant serve a very old rate silently. if the entry is older than max stale age then dont serve it, return proper 503 with our error format. and whenever we serve stale data the caller must know it clearly. add header like X-Cache: HIT / MISS / STALE, and in json body add "stale": true, the rates date and age in seconds. also set Cache-Control headers to the client properly
>
> 5. singleflight, so if 100 requests come at same time for same key on a cache miss only one call goes to frankfurter and others wait for that result. explain how this protects the vendor and our breaker from a thundering herd, like after a restart or cache expiry
>
> 6. limit the cache size (max entries from env) with LRU eviction, so memory cant grow forever. also short negative caching for invalid or unsupported currency from vendor, so someone spamming bad currency doesnt hit frankfurter every time. keep negative TTL small
>
> 7. /ready should still return 200 like we decided in phase 2, but add cache info in the body, like entry count and age of oldest entry. if you think readiness should behave differently now that we have stale data, explain it but dont change the 200 behavior without telling me
>
> 8. hooks for hit, miss, stale, eviction and singleflight shared calls, so prometheus metrics plug in easy next phase. dont add metrics yet
>
> 9. for live demo i want to show stale serving when vendor goes down without restarting the service. since base url is fixed at startup, make a small fake vendor in cmd/fakevendor that proxys to real frankfurter and has a endpoint to toggle it into failure mode (errors or slow responses). we will reuse this for the chaos demo later. add make targets for it
>
> 10. tests with httptest and -race: hit/miss, fresh expiry, stale served when vendor fails, stale beyond max age gives 503, singleflight with many concurrent requests gives only 1 vendor call, LRU eviction, negative cache, key normalization
>
> also last time old server process was still holding port 8080 and messed up the demo. add a make stop target or a port check so this doesnt happen again
>
> update .env.example and README. at the end give me exact commands to run tests and the step by step live demo (warm cache, break fake vendor, show STALE response, show 503 after max stale), and tell me whats next

**Summary:** Added `internal/cache` (a `Cache` interface + in-memory LRU,
with the Redis-vs-in-memory multi-pod tradeoff documented) and
`internal/fxvendor/ratescache` (normalized cache keys, fresh/stale/max-age
TTL logic, `golang.org/x/sync/singleflight` deduplication, negative
caching for unknown currencies). Added `X-Cache` header and
`stale`/`date`/`age_seconds` body fields, with `Cache-Control` set per
case. Built `cmd/fakevendor` — a toggleable reverse proxy in front of
the real Frankfurter API (`mode=off|error|slow`) — for live chaos demos
without restarting `fxservice`. Added `make stop`/port-check targets.
Wrote `httptest`-based tests covering hit/miss/stale/expiry/singleflight/
LRU/negative-caching. Updated `.env.example` and README with the full
stale-serving demo walkthrough.

## Phase 4: Observability

**Prompt:**

> phase 3 done, demo worked for me too. now phase 4, observability. keep this one minimalistic, no over engineering. no tracing, no jaeger, no alertmanager, no loki. just prometheus metrics + a small docker compose stack
>
> 1. prometheus metrics in its own package (internal/metrics), plugged into the hooks we already have. what i need:
>    - RED metrics for inbound http: requests total, errors, latency histogram. label by route pattern (like /v1/rates/{base}) NOT the raw path, and never put ip, currency or amount in labels. explain cardinality in short
>    - vendor calls: latency histogram and outcome (success, error, timeout, breaker_open)
>    - retry attempts count
>    - circuit breaker state as a gauge
>    - cache hit / miss / stale / negative hit, evictions, cache entries gauge, singleflight shared calls
>    - rate limit rejections
>    - bulkhead in-flight gauge
>    - a build_info metric with version
>    keep the metric names prometheus style with fxservice_ prefix and pick sensible histogram buckets
>
> 2. multi stage Dockerfile, small final image (distroless), run as non root
>
> 3. docker-compose.yml with only: fxservice, fakevendor, prometheus, grafana. pin image versions, add healthchecks. fxservice should call fakevendor inside compose so i can break the vendor live and watch the graphs change
>
> 4. prometheus config scraping fxservice, plus a small alert rules file (breaker open, high 5xx rate, stale serving, p99 latency high). just loaded in prometheus, no alertmanager
>
> 5. one grafana dashboard auto provisioned with the datasource, no manual clicks. few useful panels only: request rate, error rate, p50/p95/p99 latency, cache hit ratio, breaker state, vendor latency, rate limited requests. anonymous viewer login so reviewer can open it straight away
>
> 6. make targets: make up, make down, make logs, and make chaos-on / chaos-off which toggle fakevendor inside compose
>
> update README with ports and urls (service, prometheus, grafana). at the end give me exact commands to bring the stack up, generate some traffic, break the vendor and see it in grafana, and tell me whats next

**Summary:** Added `internal/metrics` — every RED/vendor/breaker/cache/
rate-limit/bulkhead/build-info metric, wired through the existing hook
structs with no package importing Prometheus except `main.go` and
`internal/metrics` itself, and a cardinality explanation (route
*patterns*, never raw paths/IPs/currencies/amounts, as labels). Added a
multi-stage `Dockerfile` producing an ~18MB distroless, non-root image,
plus the same pattern for `cmd/fakevendor`. Added `docker-compose.yml`
(fxservice + fakevendor + Prometheus + Grafana, pinned versions,
healthchecks where the image supports one). Added Prometheus scrape
config and 4 alert rules (breaker open, high 5xx, stale serving, high
p99), loaded directly with no Alertmanager. Added an auto-provisioned
Grafana dashboard (datasource + dashboard JSON, anonymous viewer
access) with the requested panels. Added `make up/down/logs/chaos-on/
chaos-off`. Verified the full stack live, including fixing a real
Frankfurter API domain migration bug found along the way.

## Phase 5: Async audit log

**Prompt:**

> phase 4 done, stack works. project is in /Users/rsharma41/SPPs, continue there. now phase 5, the bonus part: audit logging every request to postgres. keep it minimalistic like last phase, no ORM, no heavy frameworks
>
> 1. add postgres to the compose stack, pinned version (postgres 16 alpine is fine) with proper healthcheck. fxservice should wait for it but NOT crash if its down. db creds from env vars, defaults ok for local but nothing hardcoded in go code
>
> 2. use pgx/v5 with pgxpool. pool size, timeouts from env. schema migration should run automaticaly on startup, embedded sql, idempotent so running it twice is fine. keep it simple, no big migration tool unless you think its really needed
>
> 3. one audit table. store request_id, timestamp, endpoint, base/from/to currencies, amount, rate, converted amount, cache status (HIT/MISS/STALE), stale flag, http status, error code if any, latency ms. money and rates as NUMERIC not float. index on timestamp. dont store raw client ip, its PII, either skip it or store a hash. explain your choice
>
> 4. MOST IMPORTANT: writes must be async and never slow down or break the api request. use a buffered queue with a background batch writer (batch size and flush interval from env). if queue is full drop the record and count it in a metric, dont block. if postgres is down api should keep serving normally, writer retries with backoff. on graceful shutdown flush whats in the queue with a timeout. explain this tradeoff, because for a real fintech audit log dropping records might not be acceptable, tell me what you would do in prod instead (like outbox pattern or kafka) in short
>
> 5. /ready stays 200 even if db is down, same reasoning as vendor. just report db status in body
>
> 6. metrics: pgxpool stats (total, idle, acquired, max conns, acquire count, acquire wait duration, empty acquire count), audit queue depth, records written, dropped, write errors, batch write latency. add a few panels in the existing grafana dashboard and one alert rule for dropped records or db write failures
>
> 7. make targets: make db-shell, make audit-tail (shows last 10 audit rows), make db-down and make db-up so i can kill postgres live and show api still working and drop/error metrics going up
>
> 8. tests for the batch writer using a fake sink, no real db needed. cover batching, flush on interval, drop when full, flush on shutdown. -race as always
>
> update .env.example and README. at the end give me exact commands to run tests, bring up the stack, see audit rows, kill postgres and show the api still serving, and tell me whats next

**Summary:** Added `internal/audit` — an embedded, idempotent SQL schema
(`audit_log` table, `NUMERIC` columns for money/rates, index on
`occurred_at`, no client IP stored, with the privacy reasoning written
up), a non-blocking `Writer` (buffered channel queue, background batch
writer, full-jitter retry/backoff, bounded shutdown flush, drop-and-count
on overflow), and a `pgx`/`pgxpool`-backed `Sink` using `CopyFrom`. Added
Postgres to the compose stack (`postgres:16-alpine`, `pg_isready`
healthcheck, `fxservice` depending on `service_started` not
`service_healthy` so a down DB never blocks startup). Added pgxpool and
audit metrics, new Grafana panels, and an alert rule for dropped
records/write errors. Added `make db-shell/audit-tail/db-down/db-up`.
Wrote `-race`-clean tests against a fake in-memory sink covering
batching, interval flush, drop-when-full, retry, and shutdown flush — two
real bugs (a dead-code drain branch, and a `pgx.CopyFrom` binary-encoding
mismatch for `NUMERIC` columns) were caught by testing against a real
Postgres rather than stopping at the fake-sink unit tests. Documented the
outbox/Kafka tradeoff for a production-grade durable audit trail.

## Phase 6: UI, CI/CD, and one-command run

**Prompt:**

> phase 5 done and working. this is the last phase, keep it minimalistic like the last two
> goal: reviewer should clone the repo and run ONE command and get everything working (api, ui, postgres, prometheus, grafana). they shouldnt need go or anything installed except docker. it should work on any machine, mac intel, mac arm, linux
>
> 1. minimal ui
>    - single static html page with plain js and css, no react, no node, no build step
>    - embed it in the go binary with go:embed and serve it at / from fxservice itself, so no extra container and no CORS issues
>    - two small forms: get rates (base + symbols) and convert (from, to, amount)
>    - show the result plus the X-Cache status as a badge (HIT / MISS / STALE), stale flag, rates date and age
>    - show our json errors nicely (rate limited, circuit open, stale data unavailable)
>    - small footer with links to grafana, prometheus, /metrics, /ready
>    - light and dark mode is enough, nothing fancy
> 2. images on public registry
>    - dont build docker images locally anymore, my laptop is behind corporate proxy and it keeps breaking the build. we will build in CI only
>    - github actions workflow that builds fxservice and fakevendor for linux/amd64 and linux/arm64 and pushes to ghcr.io/<your-github-username>/fxservice and ghcr.io/<your-github-username>/fakevendor
>    - tag with latest and git short sha. use the built in GITHUB_TOKEN, no extra secrets
>    - also run go vet and go test -race in the same workflow before pushing, if tests fail dont push
> 3. one command run
>    - docker-compose.yml should pull the images from ghcr by default, not build. keep a separate docker-compose.build.yml override for anyone who wants to build from source
>    - image tag configurable via env var with latest as default
>    - all config has defaults inside compose so no .env file is needed to start
>    - fxservice should go through fakevendor in passthrough mode by default so chaos demo works out of the box
>    - make sure grafana and prometheus config still load fine from the repo
>    - final command should just be: docker compose up -d and then open http://localhost:8080
> 4. final cleanup
>    - README top section should be short: what this is, one command to run, urls table, 30 second demo (use ui, break vendor, see STALE, kill db, api still works). move all the long design explanations below it, dont delete them
>    - a small architecture diagram in the README using mermaid
>    - create PROMPTS.md with one section per phase (1 to 6) and empty placeholders, i will paste the prompts and responses myself
>    - remove any leftover temp files, dead code or comments from earlier phases
>    - run go vet, gofmt and all tests one last time
>
> at the end tell me exactly what i need to do manually (create github repo, push, make the ghcr packages public etc) and how to test the one command run on a clean machine

**Summary:** Added `internal/webui/static/index.html` — a single static
page (plain HTML/CSS/vanilla JS, no build step) with two forms (Get
rates, Convert) that call the real API via same-origin `fetch()`.
Renders an `X-Cache: HIT/MISS/STALE` badge, a stale-data badge with age,
and friendly labels for JSON error codes, plus a footer linking to
Grafana, Prometheus, `/metrics`, and `/ready`; supports light/dark via
`prefers-color-scheme`. Added `internal/webui/webui.go` embedding that
file with `go:embed` and serving it from `fxservice` at `/` — no extra
container, no CORS config needed. Added
`.github/workflows/docker-publish.yml`: a `test` job (`go vet` +
`go test -race`) gating a `publish` job that builds `fxservice` and
`fakevendor` for `linux/amd64` + `linux/arm64` via Buildx/QEMU and pushes
both to GHCR, tagged `latest` and the commit's short SHA, using only the
built-in `GITHUB_TOKEN`. Rewrote `docker-compose.yml` to pull from GHCR
by default (`IMAGE_OWNER`/`IMAGE_TAG` env vars, defaulting to `latest`,
no `.env` file required) and added `docker-compose.build.yml` as a
build-from-source override. Confirmed `fakevendor` defaults to
passthrough mode so the stack shows real rates immediately. Rewrote the
top of `README.md` into a short "what it is / one command / URL table /
30-second demo / Mermaid architecture diagram" section, moving all prior
detailed design writeups below a divider rather than deleting them.
Created this file. Cleaned up a stray `.DS_Store` and a duplicated
paragraph left over from an earlier edit. Verified `gofmt`, `go vet`, and
`go test -race ./...` all pass, and validated both compose files and the
GitHub Actions YAML. Verified the whole stack live in a local Docker
environment: UI rendering in light and dark mode, a real convert call
going from `MISS` to `HIT`, the chaos demo tripping the breaker and
showing `CIRCUIT_OPEN` in the UI, and `make db-down` with `/ready`
correctly staying `200` while the API kept serving normally.

# omlx-top

An htop-style terminal monitor for a local [oMLX](https://github.com/jundot/omlx) server,
focused on performance and bottlenecks.

```
go build -o omlx-top . && ./omlx-top
```

With a local server no flags are needed: host, port, API key, log and cache paths come from
the oMLX base directory (`~/.omlx`).

## Views

| Key | View | Contents |
|---|---|---|
| `1` | overview | Ranked bottlenecks with evidence and a suggested fix, live requests, throughput sparklines (decode, prefill, queue, GPU, memory), first-token vs generation time, finished requests |
| `2` | requests | Queued / prefill / generating requests with progress, speed and ETA; finished requests with queue wait, TTFT, tok/s, cache reuse; HTTP errors |
| `3` | models | Every discovered model: state, engine, size, context, active/waiting, idle/TTL; 7-day figures and load times |
| `4` | cache | Hot (RAM) and SSD KV cache tiers, prompt reuse, per-model hits, loads, evictions, write drops and errors |
| `5` | events | Warnings, errors, model loads, evictions, throttling and memory pressure from the server log |
| `6` | usage | Hourly or 7-day token history and per-model speeds (`u` switches range) |

The top lines always show CPU, GPU, RAM, oMLX memory against its ceiling, swap, and (if
[macmon](https://github.com/vladkens/macmon) is installed) GPU temperature, power and clock.

Keys: `q` quit · `1-6`/`tab` views · `j/k` select or scroll · `p` pause · `r` reset local
history · `u` usage range · `?` help · `esc` overview

## Bottlenecks detected

| Area | Condition |
|---|---|
| server | unreachable, a non-oMLX server on the port, slow responses, stale live data, still loading |
| memory | soft/hard pressure, near ceiling, active swapping, macOS pressure, Ollama holding memory, aborts, requeues, prefill throttling, guard rejections, Metal cap below the ceiling |
| models | eviction churn, cold loads in progress |
| queue / stall | sustained queueing, admission stalls, generation or prefill not progressing |
| speed / gpu / thermal | decode below half the model's median, GPU idle while busy, GPU temperature |
| cache | disabled, low reuse, diverging prompt prefixes, SSD errors, slow or limited SSD writes, low disk, hot cache off |
| workload / clients | first-token time dominating, replies cut at max tokens, repeated failing requests |

Logged symptoms stay listed for 10 minutes. The thresholds are heuristics, defined at the
top of `bottleneck.go`.

## Data sources

All HTTP calls are `GET`s; omlx-top changes nothing on the server.

| Source | Used for | Interval |
|---|---|---|
| `/health` | server state, memory ceiling | 1s |
| `/admin/api/activity` | in-flight requests, memory pressure | 1s |
| `/admin/api/stats` | session counters, cache statistics | 3s |
| `/v1/models/status` | model list | 10s |
| `/admin/api/usage` | stored history | 30s |
| `/api/status` | version; totals when the admin API is unavailable | 60s |
| `<base>/logs/server.log` | completions, loads, evictions, memory and cache events, errors | follow |
| `vm_stat`, `sysctl`, `ioreg`, `ps`, `statfs`, `macmon` | machine state | 1s |

The header shows how much is available: **data full** (admin API), **basic** (an inference
sub key: totals only) or **health+log only**.

## API key

The admin API needs oMLX's **main** API key. It is taken from `-api-key`, then
`$OMLX_API_KEY`, then `settings.json`. Prefer the last two: flags are visible in `ps`.

- The `settings.json` key is only sent to the local server on its configured port.
- No key is sent until `/health` identifies the server as oMLX, so another program on the
  port (such as Ollama on 11434) does not receive it.
- Redirects are not followed and HTTP proxies are not used.
- After a rejected key, authenticated calls pause for a minute.

## Flags

```
-host URL       oMLX URL or host:port      -base DIR    oMLX base directory
-api-key KEY    main API key               -log PATH    server log
-i 500ms        screen refresh             -poll 1s     API / system poll interval
-backlog N      bytes of log to replay     -macmon=false
-view NAME      starting view              -once        print one frame and exit
-width, -height size for -once
```

## Limits

- The server logs requests only when they finish. Live progress, queue wait and cached
  tokens come from polling, so requests shorter than the poll interval can show `-`.
- TTFT is in the log only for streaming chat completions. A `~` value is estimated from
  observed queue and prefill time. `?` under FINISH means no completion line was matched.
- Some APIs log no prompt token count.
- "First token" time in *where time goes* includes queueing.
- Endpoint formats and log messages follow oMLX 0.8.3; a newer release may change them.
- System statistics are macOS only.

## Test

```
go test ./...
```

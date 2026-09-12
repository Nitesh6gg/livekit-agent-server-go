# Setup

How to build and run the worker against the LiveKit server on `36.20`.

## Prerequisites

- API keys: Sarvam, Gemini.
- Network access to the LiveKit box (`36.20`, port `7880` for WS signalling plus
  the UDP range for media).
- For a **native** build: Linux amd64 with CGO, libopus and libsoxr. The audio
  path is a CGO binding, so the `livekit`/`agent`/`main` packages do **not**
  build on the Windows dev box — only the pure-Go packages do. See
  [ADR-005](DECISIONS.md#adr-005).

## 1. Configure

```bash
cp .env.example .env
```

Fill in:
- `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` — from the LiveKit
  server config on 36.20.
- `LIVEKIT_ROOM` — the room the worker joins.
- `SARVAM_API_KEY`, `GEMINI_API_KEY`.

> **Check the TTS voice matches the model family.** `bulbul:v3` uses `shubh`,
> `ritu`, `priya`, `kavya`…; `anushka` belongs to `bulbul:v2`. The template
> ships the verified `bulbul:v3` + `shubh` pairing.

> `.env.example` holds **placeholders only**. Never commit a filled-in `.env`.

## 2. Run with Docker (recommended)

Docker handles the CGO toolchain for you and works on any host:

```bash
docker compose up --build
```

Compose runs with `network_mode: host` because the worker is a WebRTC
participant doing its own ICE/RTP — bridged networking breaks the UDP media
path. See [ADR-007](DECISIONS.md#adr-007).

Ops endpoints are on `:8080` (`/healthz`, `/metrics`), configurable via
`HTTP_ADDR`.

## 3. Native build (Linux)

```bash
sudo apt-get install -y build-essential pkg-config \
    libopus-dev libopusfile-dev libsoxr-dev
export CGO_ENABLED=1
go build ./...
make build     # -> bin/agent
make run
```

`libsoxr-dev` is required — `livekit/media-sdk` pkg-configs against `soxr` for
resampling and the build fails without it.

On the Windows dev box you can still verify the pure-Go packages. Note
`internal/audio` is **no longer** among them — it needs CGO for onnxruntime
([ADR-013](DECISIONS.md#adr-013)):

```bash
go build ./config/... ./internal/stt/... ./internal/tts/... ./internal/llm/...
go vet   ./config/... ./internal/stt/... ./internal/tts/... ./internal/llm/...
```

Run the full test suite in the Docker build stage instead:

```bash
docker build --target build -t go-agent-worker:teststage .
docker run --rm go-agent-worker:teststage go test ./internal/... ./config/...
```

## Verifying the VAD on a new host

sherpa-onnx's libraries are **linked, not dlopened**, so a host missing them
fails at process start with `error while loading shared libraries:
libsherpa-onnx-c-api.so`. Check that everything resolves:

```bash
docker run --rm --entrypoint ldd go-agent-worker:local ./agent | grep "not found"
```

Empty output is a pass. The libraries ship inside the Go module and are copied
into the runtime image by the [Dockerfile](../Dockerfile); there is no separate
onnxruntime to install or version-match any more (see
[ADR-018](DECISIONS.md#adr-018)).

The models themselves are exercised by the `internal/audio` tests, which load
the vendored `ten-vad.onnx` and `gtcrn_simple.onnx` and skip if either is
missing:

```bash
docker build --target build -t go-agent-worker:teststage .
docker run --rm go-agent-worker:teststage go test ./internal/audio/ -v
```

## 4. Talk to the agent

The worker joins as `go-agent-worker`. To speak to it you need a **second**
identity with its own token:

```bash
go run ./cmd/minttoken -identity human-caller
```

Flags: `-room` (defaults to `LIVEKIT_ROOM`), `-valid-for` (default 1h).

Then open [meet.livekit.io](https://meet.livekit.io), enter the server URL
(`ws://192.168.36.20:7880`) and paste the token.

> **Windows dev box:** Application Control blocks unsigned executables from
> user-writable paths. That covers `bin\*.exe` *and* the temporary binary
> `go run` builds into `%TEMP%\go-build*`, so `go run` can fail with
> `An Application Control policy has blocked this file`. Point the build at a
> permitted path instead:
>
> ```powershell
> $env:GOTMPDIR = "D:\gotmp"   # a path the policy permits
> go run ./cmd/minttoken -identity human-caller
> ```

## 5. Reading the logs

The pipeline logs a full conversation trace:

```
stt: connected (model=saaras:v3 lang=hi-IN rate=16000 mode=transcribe)
local VAD: ten-vad threshold=0.50 minSpeech=200ms hangover=550ms
speech enhancement disabled by config (GTCRN_ENABLED)
agent: barge-in driven by local TEN VAD
vad: SPEECH_START
vad: SPEECH_END
stt: START_SPEECH          <- Sarvam's signal, ignored when local VAD is active
stt: END_SPEECH
stt: TRANSCRIPT "..."
agent: turn 1 start — user: "..."
agent: turn 1 speak: "..."
agent: turn 1 complete — reply: "..."
agent: barge-in — interrupted agent speech
agent: barge-in ignored — agent not speaking
```

If onnxruntime or the model cannot be loaded you will instead see
`local VAD unavailable, falling back to Sarvam VAD: …` followed by
`agent: barge-in driven by Sarvam server-side VAD`. The agent still runs, but
background conversation can hijack turns again.

Provider problems surface as `sarvam stt error:` / `sarvam tts error:`,
`sarvam stt|tts reconnected`, and `tts audio buffer full, dropped N chunk(s)`.

## Troubleshooting

**`token has invalid claims: token is not valid yet`** — the LiveKit server's
clock is behind this machine's, so the token's `nbf` is in its future. The
`36.20` box has drifted repeatedly because outbound NTP (UDP/123) is firewalled
and `systemd-timesyncd` cannot reach any server. Symptom check:

```bash
echo "server: $(date -u)"; echo "external: $(curl -sI https://www.google.com | grep -i '^date:')"
```

One-shot fix (drifts again — it is not a durable solution):

```bash
sudo date -u -s "$(curl -sI https://www.google.com | grep -i '^date:' | cut -d' ' -f2-)"
```

A durable fix needs either outbound UDP/123 opened, or an HTTPS-based sync
(`htpdate`) since HTTPS is reachable.

**Agent joins but never speaks** — check for `sarvam tts error:` in the logs.

**No transcripts** — check for `sarvam stt error:` and `sarvam stt reconnected`.
Repeated reconnects mean the socket is being dropped; see
[ADR-010](DECISIONS.md#adr-010).

## Load testing (later)

Media-plane load used `livekit-cli load-test`. For the **agent** plane you must
drive N concurrent callers whose audio actually contains **speech** (not
silence), so STT/turn/TTS are exercised. See
[METRICS.md](METRICS.md#how-to-generate-load).

## Notes

- Run the worker on a **separate box** from the LiveKit SFU so agent CPU/RAM is
  measured cleanly.
- Keep the OS tuning from the SFU work (fd limits, UDP buffers) on the agent box
  too — it opens many sockets per call.

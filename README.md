# go-agent-worker

A **Go agent worker** for a LiveKit-based voice AI platform. It joins a LiveKit
room and runs a real-time **STT → LLM → TTS** conversation loop with barge-in,
using native goroutines instead of a Python/Node event loop.

> **Status: Phase 1 — measurement spike, not production.**
> The pipeline is **implemented and holds real conversations**. What it cannot
> yet do is answer the question it exists for:
> **how many concurrent full agent pipelines can one vCPU run, at acceptable
> latency?** That needs instrumentation plus local VAD — see
> [docs/METRICS.md](docs/METRICS.md) and [docs/ROADMAP.md](docs/ROADMAP.md).

## Two planes (read this first)

Capacity has two independent parts. Do not conflate them:

| Plane | What it does | Where | Status |
| --- | --- | --- | --- |
| **Media plane** | LiveKit SFU forwards RTP audio | server `36.20` | ✅ Benchmarked: ~1,500 concurrent 1-on-1 media sessions/box |
| **Agent plane** | STT + LLM + TTS + turn + interruption **per call** | this worker | ❓ **Unmeasured — that's this project's job** |

A forwarded audio stream is **not** a conversation. This worker measures the
expensive plane.

## Stack

- **LiveKit:** official `livekit/server-sdk-go/v2` (join room, read/write tracks).
  No agent job-dispatch yet — the worker joins a room manually. See [ADR-001](docs/DECISIONS.md).
- **STT:** Sarvam `saaras:v3` (WebSocket)
- **LLM:** Gemini (`google.golang.org/genai`)
- **TTS:** Sarvam `bulbul:v3` (WebSocket)

Exact models come from `.env`; see [.env.example](.env.example).

## Layout

```
cmd/agent/          entry point
cmd/minttoken/      dev helper: mint a LiveKit token to join as a test caller
internal/
  agent/            the pipeline + turn/interruption orchestration
  livekit/          room join, track read/write (CGO: libopus, libsoxr)
  stt/ llm/ tts/    provider clients (Sarvam / Gemini / Sarvam)
  audio/            TEN VAD + GTCRN enhancement (CGO: sherpa-onnx)
models/             vendored ten-vad.onnx, gtcrn_simple.onnx
config/             env config loading
docs/               architecture, roadmap, metrics, decisions, setup
```

## Quickstart (Docker — recommended)

The full build needs CGO + libopus + libsoxr on Linux, so Docker is the simplest
path on any host:

```bash
cp .env.example .env      # fill in LiveKit + Sarvam + Gemini values
docker compose up --build
```

To talk to the agent, mint a token for a second identity and join via
[meet.livekit.io](https://meet.livekit.io):

```bash
go run ./cmd/minttoken -identity human-caller
```

Native (Linux) build instructions are in [docs/SETUP.md](docs/SETUP.md).

## What works, and what doesn't

**Works:** room join, caller audio → Sarvam STT, Gemini streaming, sentence
chunking → Sarvam TTS, audio published back, barge-in that cancels an in-flight
turn, automatic reconnect of both Sarvam sockets, **local TEN VAD** driving
interruptions with a minimum-speech guard, and optional **GTCRN** speech
enhancement (off by default).

**Known gaps** (see [ARCHITECTURE.md](docs/ARCHITECTURE.md#gap-analysis-vs-livekitagents)):

- **No turn-detector model.** Turn *end* is still Sarvam's `END_SPEECH`; there
  are no endpointing delays. LiveKit's own turn detector is proprietary and its
  open-weights alternative is deprecated, so there is no portable model to adopt
  — see [ROADMAP.md](docs/ROADMAP.md).
- **The STT bill is unchanged.** The VAD decides interruptions but does not gate
  the STT stream ([ADR-014](docs/DECISIONS.md)), so silence is still streamed
  and billed.
- **No per-stage metrics**, so Decision Gate 1 cannot be answered yet.
- No job dispatch, function tools, or transcription forwarding.
- **TEN VAD runs without the model's pitch feature** — sherpa substitutes zero,
  costing ~14% extra false speech frames against the official implementation,
  whose pitch algorithm is not published
  ([ADR-018](docs/DECISIONS.md#adr-018)).
- **GTCRN does not remove distant background talkers.** It suppresses noise and
  is trained to preserve speech; a person across the room is speech. That needs
  interruption policy (`min_words`), not denoising.
- **Neither model has been run against a live call yet.** Both are verified
  against recorded and synthetic audio only.

## Docs

- [ARCHITECTURE.md](docs/ARCHITECTURE.md) — pipeline, goroutines/channels, barge-in, gap analysis vs `livekit/agents`
- [ROADMAP.md](docs/ROADMAP.md) — phases and the decision gates between them
- [METRICS.md](docs/METRICS.md) — what to measure and the pass/fail thresholds
- [DECISIONS.md](docs/DECISIONS.md) — architecture decision records
- [SETUP.md](docs/SETUP.md) — building and running against the LiveKit box on 36.20

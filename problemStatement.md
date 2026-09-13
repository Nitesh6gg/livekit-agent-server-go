# Problem Statement — Go Voice Agent Worker

> [!IMPORTANT]
> **Historical document, kept for provenance.** This is the original HLD. Several of
> its load-bearing numbers and technology choices have since been superseded by
> measurement — most importantly the density arithmetic in §3A, which the entire cost
> case rests on. Corrections are tabulated below. The authoritative record is
> [docs/DECISIONS.md](docs/DECISIONS.md) and [docs/ROADMAP.md](docs/ROADMAP.md).

## Superseded by measurement

| This document says | Current position | Record |
|---|---|---|
| Go workers reach **100+ calls/vCPU** (§3A), cutting compute ~90% | Unsupported by any measurement. The incumbent Node/LiveKit stack measures **7.07 calls/core** at 85 concurrent rooms; the bar Go must clear is **≥14**. The 2–4 KB goroutine-stack argument addresses *memory*, not CPU — and per-call CPU is dominated by ONNX inference and Opus codec work in native C, identical in either language. | [ADR-019](docs/DECISIONS.md#adr-019) |
| Pipecat is limited to **3–4 calls/process** (§1) — and separately, **10–12 calls/vCPU** (§3A) | Internally inconsistent, and both understate it. Dograh/Pipecat measures **5–6 calls/core**. Understating the incumbent inflates the case for the rewrite. | [ADR-019](docs/DECISIONS.md#adr-019) |
| Agent worker pool built on **`am-sokolov/livekit-agent-sdk-go`** (§2, §3B) | Rejected before implementation, and the deferred revisit is now closed: last commit ~11 months ago, 8 stars, 4 forks. The official `server-sdk-go` is used instead, and there is **no job dispatch at all yet** — the worker joins rooms manually. | [ADR-001](docs/DECISIONS.md#adr-001) |
| STT via **Deepgram/AssemblyAI**, TTS via **Cartesia/ElevenLabs** (§3B) | Sarvam for both. The product is Indian-language voice, and measuring on English-optimised providers produces latency and quality numbers that do not transfer to production. §4 of this document already says Sarvam. | [ADR-002](docs/DECISIONS.md#adr-002) |
| The pipeline is **4 goroutines** (§3B) | Missing the stage that matters most for density: in-process TEN VAD, optionally with GTCRN speech enhancement. That native inference is a large share of per-call CPU, and is the main reason §3A's figure does not hold. | [ADR-018](docs/DECISIONS.md#adr-018) |
| Barge-in **cancels streams in <10 ms** (§3C) | Unmeasured, and not how it works. TTS audio is filtered by generation at playback rather than stopped at the source, gated on real playout state — cancelling the stream alone does not stop audio already in flight. | [ADR-015](docs/DECISIONS.md#adr-015) |
| Target **sub-500 ms** end-to-end latency (§4) | Not met by either stack. Gate 1 now uses Node parity (~2,427 ms p95) as its condition; the earlier ≤800 ms placeholder was unmet by both stacks and so could not discriminate between them. | [METRICS.md](docs/METRICS.md) |

**What still stands:** the 5,000-call target, the three-layer decoupling, and the
phased structure. What changed is the density arithmetic underneath them — and Gate 1
is now explicitly allowed to kill the rewrite if measurement does not support it.

---

**Contents:** [1. Core Problem & Goal](#core-problem) · [2. Selected Tech Stack Overview](#tech-stack) · [3. Key Technical Decisions & Architectural Insights](#key-decisions) · [4. Phased Rollout Roadmap](#roadmap)

---

<a id="core-problem"></a>

## 1. Core Problem & Goal

* **Current Bottleneck:** Using Pipecat (Python), your platform is limited to **3–4 concurrent calls per process** due to single-threaded event loops, heavy audio framing overhead, and Python's Global Interpreter Lock (GIL).
* **Target Scale:** Build a self-hosted voice platform capable of handling **5,000 concurrent calls** while starting with **Phase 1 (1–50 calls)** for rapid validation.

---

<a id="tech-stack"></a>

## 2. Selected Tech Stack Overview

To eliminate event-loop bottlenecks and achieve maximum concurrency, we settled on a **100% Go-centric architecture** decoupled across three distinct layers:

| Layer | Selected Tech / Framework | Purpose & Justification |
| --- | --- | --- |
| **Media Gateway** | **LiveKit Server (Go)** | Handles WebRTC audio routing, ICE/TURN, and room management. Does zero AI processing to maintain ultra-low latency. |
| **Server API (Control)** | **Go (Chi)** + **`sqlc`** / **`pgx`** + **Genkit Go** | • **Chi:** Lightweight, zero-allocation HTTP router for tokens & webhooks.<br>• **`sqlc`:** Type-safe, compile-time SQL with zero reflection overhead.<br>• **Genkit Go:** Manages prompts (`Dotprompt`), RAG retrievers, and developer UI tracing offline. |
| **Agent Worker Pool** | **Go (`am-sokolov/livekit-agent-sdk-go`)** | Low-level protocol worker running native Goroutines for audio streaming. Bypasses Python/Node.js event loops entirely. |
| **Database & Vector** | **PostgreSQL** + **`pgvector`** | Stores user data, call history, and document embeddings for sub-15ms RAG lookups. |

---

<a id="key-decisions"></a>

## 3. Key Technical Decisions & Architectural Insights

### A. The Go Concurrency Advantage (5K Scale Math)

* **Python Workers:** ~10–12 calls per vCPU → requires **~480 vCPUs** (30 × `c6g.4xlarge` nodes).
* **Go Workers:** Goroutines use 2–4 KB stack memory, yielding **100+ calls per vCPU** → requires **~50 vCPUs** (4 × `c6g.4xlarge` nodes), achieving a **~90% reduction in compute costs**.

### B. Custom Go AI Pipeline Architecture

Because `am-sokolov` provides the LiveKit protocol dispatch without high-level Python AI wrappers, your Go worker manages AI work using **4 concurrent Goroutines connected by Go Channels**:

1. **Ingestion & STT:** Streams incoming audio frames to Deepgram/AssemblyAI over WebSockets.
2. **Context & LLM:** Appends transcripts to thread-safe history (`sync.RWMutex`) and streams LLM tokens.
3. **Sentence Chunking & TTS:** Groups LLM tokens into sentences and streams them to Cartesia/ElevenLabs.
4. **LiveKit Writer:** Writes raw PCM/Opus frames back to the LiveKit audio track.

### C. Instant Interruption ("Barge-in") Handling

Instead of framework abstractions, voice interruptions are handled via **Go `context.WithCancel()`**:

* When STT detects user speech while the agent is talking, calling `cancel()` instantly terminates active LLM/TTS WebSocket streams and flushes audio buffers in **<10 milliseconds**.

### D. The Role of Genkit Go

* **Where to use Genkit:** In the **Server API layer** for prompt templates, RAG integration, admin workflows, and Developer UI debugging (`genkit start`).
* **Where to avoid Genkit:** Inside the **real-time Agent Worker loop**, where framework abstraction overhead and telemetry logging add unwanted latency to sub-300ms audio streams.

---

<a id="roadmap"></a>

## 4. Phased Rollout Roadmap

> ### 📍 Phase 1 — 1–50 Calls *(current)*
> * Run a single LiveKit Server container locally/on 1 VM via Docker.
> * Build the Go agent worker using raw WebSockets for Sarvam STT, Gemini LLM, Sarvam TTS.
> * Test voice prompt quality, interruption speed, and sub-500ms end-to-end latency.

> ### ⏳ Phase 2 — 50–500 Calls *(not started)*
> * Add a Redis instance for LiveKit room state coordination.
> * Implement worker process isolation and test SIP telephony integration.

> ### ⏳ Phase 3 — 500–5,000 Calls *(not started)*
> * Deploy LiveKit and Go Agent Workers on a Kubernetes cluster (EKS/GKE) with Horizontal Pod Autoscaling (HPA) and Linux kernel socket optimizations.

# Problem Statement — Go Voice Agent Worker

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

# Roadmap

The plan is deliberately gated: each phase must **pass a measurable gate** before
the next begins. The goal is to avoid a multi-month rewrite that is only justified
by assumptions.

## Phase 0 — Scaffold & docs ✅ done

- Project structure, config surface, and docs in place.
- **Exit:** structure agreed; implementation approved.

## Phase 1 — One call path, measured 🔄 in progress

Build the minimum that runs **one real conversation** and instrument it.

**Done:**
- Join one LiveKit room (official `server-sdk-go`).
- STT (Sarvam) → LLM (Gemini) → TTS (Sarvam) pipeline holding real
  conversations end to end.
- Barge-in via `context.WithCancel()`, gated on real playback state.
- Docker build/run path; lazy reconnect on both provider sockets; provider
  error surfacing; conversation trace logging.

**Remaining before Gate 1 can be answered:**

1. ~~**Local VAD.**~~ Done — TEN VAD runs in-process via sherpa-onnx, with
   optional GTCRN enhancement ([ADR-018](DECISIONS.md#adr-018)). Per-call CPU is
   no longer almost-entirely remote I/O, which is what makes a Gate 1 number
   transferable. **Endpointing is still Sarvam's**, so the delegation is
   reduced, not eliminated.
2. **Turn-taking policy.** Interruption minimums (duration, words) and
   endpointing delays. Without them background conversation hijacks turns —
   and this, not denoising, is the measure aimed at distant background talkers.
   Now the highest-value remaining item.
3. **Per-stage instrumentation.** Latency percentiles, TTFB, interruption
   latency, per-call CPU/RAM split by stage. None of it exists yet.
4. **Load generation** with real speech audio (see METRICS).

**Decision gate 1 (the important one):**

| Question | Pass condition |
| --- | --- |
| Is per-call latency acceptable? | p95 speech-end → agent-audio ≤ target (see METRICS) |
| What is agent density? | measured concurrent full pipelines per vCPU |
| Does Go actually beat the incumbent? | Go density ≥ 2× the **measured Node baseline of 7.07 calls/core**, i.e. ≥ 14, at equal latency |
| What does it cost? | $/concurrent-call (vendor bill) modeled at 500 and 5,000 |

> If Go does **not** clearly beat the incumbent stack on density at equal latency
> and cost, **stop** — the rewrite is not justified; scale the existing stack
> horizontally instead. This gate is allowed to kill the project.

**Amended by [ADR-019](DECISIONS.md#adr-019).** The baseline is Node at 7.07
calls/core, not Pipecat at 5–6 — the rewrite must beat the best available option,
not the one being retired. Three preconditions now apply before this gate may be
answered at all: in-process end-of-turn parity (item 1–2 below), the bar restated
above, and **item 3 reordered ahead of items 2 and 4** — a pprof CPU split of native
inference vs. orchestration bounds the achievable density before more code is
written, and can answer the gate cheaply with a "stop".

## Phase 2 — 50–500 calls

Only if Gate 1 passes.

- Agent job dispatch / worker pool (evaluate `am-sokolov` vs building on
  `server-sdk-go` — see [DECISIONS.md](DECISIONS.md#adr-001)).
- Redis for LiveKit room-state coordination.
- Process isolation and restart/health semantics.
- Begin the **SIP/telephony** investigation (this is a large, separate track;
  do not assume it is a "test").

**Decision gate 2:** stable at 500 concurrent, dispatch is reliable, telephony
path proven end-to-end for at least one carrier.

## Phase 3 — 500–5,000 calls

- Kubernetes (EKS/GKE) for LiveKit + agent workers, with HPA.
- Kernel/socket tuning at the node level.
- Separate autoscaling for the SFU fleet vs the agent fleet.
- Revisit self-hosted GPU STT/TTS vs per-minute APIs — at this scale the vendor
  bill, not compute, sets the architecture.

**Decision gate 3:** cost-per-call and latency hold at 5K in a soak test.

## What could still kill or reshape this

- Agent density comes back far below the HLD's "100/vCPU" claim.
- Telephony migration off FreeSWITCH/Asterisk proves too costly.
- Vendor bill at 5K forces self-hosted inference, changing the whole design.
- **Turn-taking parity proves too expensive to rebuild.** `livekit/agents` gets
  its quality from VAD *plus* a semantic end-of-utterance transformer. The VAD
  half is now solved from Go via sherpa-onnx; the EOU model is Python-only, and
  sherpa-onnx does **not** ship a turn detector either (Smart Turn v3 and TEN's
  turn-detection model are separate projects). If matching it needs a Python
  sidecar per call, a core premise of the Go rewrite — no Python in the hot
  path — is undermined. See
  [ARCHITECTURE.md](ARCHITECTURE.md#gap-analysis-vs-livekitagents).

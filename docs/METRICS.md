# Metrics — the Phase 1 measurement plan

This is the reason the project exists. The media plane (LiveKit SFU) is already
proven; the **agent plane is not**. Phase 1 answers, with numbers, whether the Go
agent worker is dense, fast, and cheap enough to justify the rewrite.

## Ground rule: measure the two planes separately

| Plane | Already known | Measure here? |
| --- | --- | --- |
| Media (SFU, 36.20) | ~1,500 concurrent 1-on-1 sessions/box, RAM-bound | No |
| Agent (this worker) | nothing | **Yes** |

Run the agent worker on a **dedicated box**, not on the SFU, so the two capacities
don't contaminate each other.

## ⚠️ Do not benchmark until local VAD lands

The worker currently delegates **all** voice activity detection and endpointing
to Sarvam's server-side VAD. Per-call CPU here is therefore almost entirely
remote I/O — the expensive inference runs on the vendor's machines, not ours.

Measuring in this state produces two wrong answers at once:

- **Density looks far too good**, because we are not doing the work a
  `livekit/agents` or Pipecat agent does locally (Silero VAD + EOU turn model).
  Comparing that against a Pipecat baseline is not a like-for-like test, and
  metric #1 below would report "mostly remote I/O" — which the note there
  correctly reads as *Go barely helps*.
- **Vendor cost looks too high**, because every audio second is streamed and
  billed, including silence. Observed: ~₹0.50/min per concurrent call for STT
  alone, with no local gating.

Land local VAD first (see [ROADMAP.md](ROADMAP.md)), then measure.

## The five metrics

### 1. Per-call resource cost
- **CPU** per active call (millicores) and **RAM** per active call (MB).
- Break CPU down by stage: VAD/turn vs audio codec/resample vs I/O.
- *Why:* if per-call CPU is mostly remote I/O, Go barely helps and the density
  case collapses. If it's VAD/turn inference, that's the real lever.

### 2. Agent density (headline number)
- Max **concurrent full pipelines per vCPU** at acceptable latency + quality.
- Watch **RAM as a likely ceiling** (it was on the SFU box — 86% RAM vs 71% CPU).
- *Why:* this single number drives the whole 5,000-call compute cost and the
  Go-vs-Python decision.

### 3. End-to-end latency
- **p50 / p95 / p99** from caller speech-end → first agent audio out.
- Measure **under load**, not with one idle call.
- *Why:* WebRTC was laggier than WebSocket in an earlier demo; this is the known
  risk. 0% packet loss ≠ low latency.

### 4. Interruption (barge-in) latency
- Time from caller-starts-talking (over agent) → agent audio silent.
- Measure at **idle and at high load** — does it stay snappy when the box is busy?

### 5. AI-vendor cost per concurrent call
- ₹/minute for Sarvam STT + Gemini + Sarvam TTS × realistic talk ratios.
- Extrapolate to 500 and 5,000 concurrent.
- *Why:* at 5K this dwarfs compute and can force self-hosted inference.
- **Baseline already observed (ungated):** ~₹0.50/min per call for Sarvam STT
  alone — billed by streamed audio duration, so silence costs the same as
  speech. At 5,000 concurrent that is ~₹2,500/min for STT before LLM or TTS.
  This is the *ceiling*; local VAD gating is the single biggest lever on it.

## Pass / fail thresholds

**No longer placeholders — anchored to the measured Node/LiveKit baseline.** See
[ADR-019](DECISIONS.md#adr-019). That baseline, at 85 concurrent rooms on 16 vCPU /
31 GB with 0 provider errors: **7.07 calls/core**, 258 MB/call, composite
speech-end → first-audio p95 **~2,427 ms** (of which ~900 ms is a deliberate
`minDelay` endpointing choice, not framework overhead). LiveKit's own published
sizing is ~7.9 jobs/core, so 7.07 is what that framework costs, not a
misconfiguration waiting to be tuned away.

| Metric | Target | Hard fail |
| --- | --- | --- |
| p95 end-to-end latency | ≤ 2,427 ms (Node parity) | > 2,427 ms |
| Interruption latency (under load) | ≤ 300 ms | > 700 ms |
| Agent density | **≥ 14 calls/vCPU** (2× Node) | **≤ 10 calls/vCPU** |
| Go vs **Node** density | ≥ 2× at equal latency | ≤ 1.4× |
| Cost/concurrent-call | within budget | > budget |

Three notes on reading this table, each recording a way it previously would have
given the wrong answer:

- **Density between 11 and 13 calls/vCPU is neither pass nor fail.** Re-cost the
  project against the memory saving alone rather than reading it as a pass.
- **The baseline is Node, not Pipecat.** The earlier row compared against a Pipecat
  baseline (5–6 calls/core), setting the bar at 10–12. Pipecat is no longer the
  alternative to a Go rewrite; the rewrite has to beat the *best available* option.
  At the old bar, Go could land at 11 calls/core, formally pass, and deliver 1.5×
  the stack it actually replaces.
- **The old ≤ 800 ms latency target was unmet by either stack.** Node's composite
  p95 is ~2,427 ms. Keep 800 ms as a *product* aspiration if the SLA needs it, but
  it cannot be Gate 1's pass condition: a bar the incumbent also fails cannot
  discriminate between them.

**Equal latency means the same turn-taking policy.** Go must commit turns on the
same endpointing delay and interruption minimums as the Node baseline, and must run
end-of-turn detection **in-process** — Node pays for `inference.TurnDetector()` on
the CPU being measured, while the Go worker still delegates endpointing to Sarvam.
Measured before that parity exists, Go wins partly by doing less work, and both the
density and latency numbers are void (ADR-019, precondition 1).

**RAM is co-binding with CPU and has no target row on purpose.** §2 above predicted
it and the Node run confirms it: 258 MB/call puts 85 calls at 71% of 31 GB while CPU
peaks at 90%, so a larger box buys cores that RAM cannot feed. The per-call figure
to beat is 258 MB. Setting a Go target before the profile in ADR-019 precondition 3
exists would be inventing a number — record what is measured instead.

## How to generate load

- **Media/SFU load:** `livekit-cli load-test` (already used on 36.20).
- **Agent load:** you need N concurrent *real pipelines*, each with audio that
  actually triggers STT/VAD/turn/TTS — not silence. A pre-recorded speech loop
  per simulated caller is the minimum; silence under-reports CPU badly.

## Recording template (per run)

```
run id:            ____
date / commit:     ____
box (vCPU/RAM):    ____
concurrency:       ____
p50/p95/p99 lat:   ____ / ____ / ____ ms
interruption lat:  ____ ms (idle) / ____ ms (loaded)
CPU per call:      ____ mcpu   (VAD ___ / codec ___ / io ___)
RAM per call:      ____ MB
density:           ____ calls/vCPU   (ceiling: CPU / RAM?)
vendor $/call/min: ____
verdict:           pass / fail / needs-tuning
```

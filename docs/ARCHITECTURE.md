# Architecture

## Purpose

A Go worker that runs one real-time voice conversation per call: it reads the
caller's audio from a LiveKit room, transcribes it, gets an LLM reply, speaks it
back, and stops when the caller interrupts.

The worker exists to be **measured** in Phase 1 (see [METRICS.md](METRICS.md)),
not to be shipped. The pipeline described below is **implemented and running**;
the gaps that stop it being a real agent framework are catalogued in
[Gap analysis](#gap-analysis-vs-livekitagents).

## The two-plane model

```
 Caller ──WebRTC──▶  LiveKit SFU (36.20)  ──▶  Go agent worker (this repo)
                     [media plane]              [agent plane]
                     forwards audio             STT + LLM + TTS + turn
```

- **Media plane = LiveKit SFU.** Already benchmarked (~1,500 concurrent 1-on-1
  sessions on a 32-core box). Does **zero** AI work. Not this repo.
- **Agent plane = this worker.** Where the per-call CPU and the AI-vendor cost
  actually live. Runs on a **separate fleet** from the SFU.

Keeping these separate is the whole point: a forwarded audio stream is not a
conversation, and their capacities are independent.

## The pipeline

One call = one `context.Context` plus a set of goroutines communicating over
buffered channels.

```
 caller audio ──▶ [1] STT ──events──▶ [2] Pipeline.Run ──▶ [3] LLM ──sentences──▶ [4] TTS
                                             │                                      │
                                             └──────── [5] playbackLoop ◀───PCM──────┘
                                                            │
                                                            ▼
                                                    LiveKit audio track
```

1. **`internal/livekit`** subscribes to the caller's track, decodes Opus → PCM16
   and resamples to the STT rate, handing chunks to `OnCallerAudio`.
2. **`internal/stt`** streams that PCM to Sarvam and emits `START_SPEECH`,
   `END_SPEECH` and transcripts on **one ordered channel** (see below).
3. **`internal/agent`** (`Pipeline.Run`) consumes those events and drives turns.
4. **`internal/llm`** streams Gemini tokens; the pipeline chunks them into
   sentences and hands each to **`internal/tts`**.
5. **`playbackLoop`** forwards decoded TTS PCM back to the published track.

### Why STT events share one channel

Transcripts and VAD signals arrive on a **single ordered channel**, not separate
ones. This is load-bearing, not stylistic. Go's `select` picks uniformly at
random among ready cases, so two channels reorder events that Sarvam sent in
sequence — and a `START_SPEECH` processed *after* its own transcript cancels the
turn that transcript just created. That desync is self-sustaining: every later
turn is then killed by the previous utterance's signal, and the agent goes
permanently silent. `livekit/agents` routes both through one ordered handler for
the same reason. See [ADR-009](DECISIONS.md#adr-009).

## Turn and interruption model

Interruption uses **`context.WithCancel()`**, not a framework abstraction:

- Each agent *turn* runs under a per-turn context derived from the call context.
- A barge-in calls `cancel()`, which propagates to the Gemini stream and stops
  further TTS for that turn.
- Queued audio is dropped from both the LiveKit playout queue
  (`FlushPlayback`) and the decoded-TTS channel (`drainAudio`).

**`isSpeaking()` is the gate for all of this.** A finished LLM stream does *not*
mean the agent stopped talking: a reply is handed to TTS in one go but plays out
over many seconds. The pipeline therefore treats the agent as speaking if either
a turn is in flight **or** audio was written within `speakingIdleGap` (500ms).
Barge-in is ignored when the agent is not speaking — otherwise room noise mutes
a reply that hasn't started yet. This is a hand-rolled stand-in for
`livekit/agents`' `SpeechHandle`.

### Local VAD (TEN VAD)

Barge-in is triggered by **TEN VAD** running in-process (`internal/audio`, via
sherpa-onnx), not by Sarvam's `START_SPEECH`. Sarvam's signal fires on
background conversation and carries a network round trip; local inference does
neither. Sarvam's `END_SPEECH` still ends a caller turn.

The detector runs on its own goroutine — `Submit` never blocks, because it is
called from the LiveKit decode goroutine. Audio is accumulated into the
256-sample windows TEN VAD is fed at 16 kHz (one inference per 16 ms), which is
also GTCRN's frame shift, so the two need no re-buffering between them.

- `VAD_MIN_SPEECH_MS` (200ms) — speech must persist this long before it
  interrupts. This is the guard against a cough or a door closing.
- `VAD_HANGOVER_MS` (550ms) — silence required before speech is considered over,
  so a mid-sentence pause does not end the turn.
- `VAD_THRESHOLD` (0.5) — speech probability.

If the model cannot be loaded, the pipeline logs it and falls back to Sarvam's
VAD rather than refusing to start.

**One caveat worth knowing when reading logs:** sherpa's TEN VAD does not
compute the model's pitch feature, substituting zero. Measured against the
official TEN VAD, that costs ~14% extra false speech frames — and the official
pitch algorithm is not published, so this is the only implementation available.
See [ADR-018](DECISIONS.md#adr-018).

**The VAD does not gate what is sent to Sarvam** — the STT socket stays
continuous. See [ADR-014](DECISIONS.md#adr-014); the STT bill is unchanged.

**Still missing:** endpointing delays (`min_delay`/`max_delay`) and a
`min_words` interruption threshold. Turn *end* is still entirely Sarvam's
decision. `min_words` is the measure most directly aimed at distant background
talkers, which denoising does not solve.

The VAD drives *when* to interrupt; it does not drive the latency numbers.
Speech-end for metrics comes from Sarvam's own `occured_at` timestamp instead
(see [Providers](#providers) below and [ADR-017](DECISIONS.md#adr-017)) — local
VAD leads Sarvam by ~200ms and looked like the better reference, but using it
required correlating two independently-clocked signals, which is what broke
twice under [ADR-016](DECISIONS.md#adr-016).

On a genuine mid-speech interruption (`isSpeaking()` was true), `onBargeIn`
also disconnects the TTS socket — see below.

### Speech enhancement (GTCRN, optional)

When `GTCRN_ENABLED=true`, caller audio is denoised before it reaches **both**
the VAD and Sarvam STT. It runs inline on the LiveKit decode goroutine so the
two see the same stream in the same order — roughly 0.07 real-time, about 1 ms
of work per 16 ms frame.

**Off by default**, for two reasons: it adds one frame (16 ms, measured) of
delay to the STT path, and altering what Sarvam hears carries real risk — ASR is
trained on unprocessed audio, and [ADR-012](DECISIONS.md#adr-012) is the local
precedent for a well-meaning audio change costing transcripts.

**What it does not do:** GTCRN suppresses *noise* — fans, traffic, hum. It is
trained to preserve speech, so a second person talking across the room is not
removed; their voice is speech, and the model cannot know it is not the
caller's. Suppressing an interfering talker needs target-speaker extraction or
diarization, which this is not. See [ADR-018](DECISIONS.md#adr-018).

## Providers

| Role | Provider | How it's reached |
| --- | --- | --- |
| STT | Sarvam `saaras:v3` | WebSocket streaming API (no Go SDK) |
| LLM | Gemini | `google.golang.org/genai` |
| TTS | Sarvam `bulbul:v3` | WebSocket streaming API (no Go SDK) |

Exact model/voice IDs come from `.env`. The TTS speaker must belong to the
model's family (`bulbul:v3` uses `shubh`, `ritu`, `priya`…; `anushka` is a v2
voice).

Both Sarvam clients follow the same connection contract, learned the hard way:

- The TTS URL **must** carry `send_completion_event=true`, or Sarvam accepts
  every message and silently emits no audio ([ADR-008](DECISIONS.md#adr-008)).
- The STT socket **must not** be sent a keepalive — any message without an
  `audio` field is rejected and the socket closed ([ADR-010](DECISIONS.md#adr-010)).
- Sockets are redialed **lazily on the next send**, never from a background
  retry loop ([ADR-011](DECISIONS.md#adr-011)).
- Provider error frames are logged. Sarvam reports STT failures in
  `data.message`, not `data.error`; reading only the latter hid every STT error.
- TTS audio is filtered by a per-turn generation tag before playback, since
  Sarvam has no way to cancel an in-flight synthesis request and keeps
  streaming a cancelled turn's audio regardless
  ([ADR-015](DECISIONS.md#adr-015)). On top of that, a genuine mid-speech
  interruption also disconnects and lazily redials the TTS socket — Sarvam's
  own "done" signal for the abandoned request (`final`) was observed taking up
  to 9.5s in a live call, long enough for a fresh reply's real audio to be
  mistagged as stale and dropped ([ADR-017](DECISIONS.md#adr-017)).
- STT's `START_SPEECH`/`END_SPEECH` events carry Sarvam's own `occured_at`
  timestamp — undocumented, previously unparsed. It is **not** used for latency
  maths: a live call measured Sarvam's clock 3.87s from ours (stable to 35ms
  across nine turns), and even corrected it is a *detection* time landing ~400ms
  after the acoustic speech-end. Speech-end is the **local receipt time** of
  `END_SPEECH`; the ordering guarantee above is what makes that safe, and
  `occured_at` is kept only to report clock skew
  ([ADR-017](DECISIONS.md#adr-017)).

## LiveKit integration

Uses the **official** `livekit/server-sdk-go/v2`. The worker joins a room as a
participant, subscribes to the caller's track, and publishes its own. There is
**no agent job-dispatch / worker-pool** — rooms are joined manually. Rationale in
[ADR-001](DECISIONS.md#adr-001).

The media path is CGO: `pkg/media` → `media-sdk/opus` (libopus) and libsoxr for
resampling. See [ADR-005](DECISIONS.md#adr-005) and [ADR-007](DECISIONS.md#adr-007).

## Gap analysis vs `livekit/agents`

`livekit/agents` (Python) is the reference implementation of this problem. Naming
its components makes our gaps concrete rather than vague.

| Component | `livekit/agents` | This worker | Gap |
| --- | --- | --- | --- |
| Job dispatch | `AgentServer`: registration, capacity reporting, load balancing, graceful drain, ~15s crash re-dispatch | manual room join | **Missing** (Phase 2) |
| Session container | `AgentSession` | `agent.Pipeline` | partial |
| Agent definition | `Agent` — instructions + tools | `const systemPrompt` | minimal |
| Speech state | `SpeechHandle` — state, `.interrupted`, `current_speech`, callbacks | `isSpeaking()` heuristic | **weak** |
| VAD | **Silero** (ONNX, local CPU, prewarmed per server) | **TEN VAD** (via `sherpa-onnx-go`) | ✅ |
| Speech enhancement | not built in | **GTCRN** (via `sherpa-onnx-go`, opt-in) | ahead |
| Turn detection | EOU transformer + `min_delay`/`max_delay` endpointing | Sarvam `END_SPEECH` only | **Missing** |
| Interruption policy | `enabled`, `mode`, `min_duration`, `min_words`, `discard_audio_if_uninterruptible` | every `START_SPEECH` | **Missing** |
| Audio I/O | `RoomIO`, `AudioInputOptions`, built-in AGC | `livekit.Session` | partial |
| Provider plugins | swappable plugin interfaces | concrete structs | partial |
| Function tools | `@function_tool` | none | missing |
| Chat context | `ChatContext` | `agent.History` | ✅ |
| Metrics | usage/latency collection | goroutines + memstats only | **Missing** |
| Transcription sync | forwarded and synced to playback | none | missing |

### The consequence that matters most

**We offload VAD and endpointing to Sarvam; `livekit/agents` and Pipecat run
them locally.** Two effects:

1. **Cost.** Sarvam must receive every audio second to do VAD on our behalf.
   LiveKit's own docs give the counter-model: VAD "helps optimize resource usage
   by only performing speech-to-text while the user speaks."
2. **The headline metric would be misleading.** [METRICS.md](METRICS.md) asks to
   split per-call CPU into VAD/turn vs codec vs I/O, and warns that if it is
   mostly remote I/O then Go barely helps. Today ours is *almost entirely*
   remote I/O, because the expensive inference runs on Sarvam's machines.
   Benchmarked as-is, Go would look excellent against a Pipecat agent that runs
   Silero + EOU locally — and the comparison would be meaningless.

Closing this gap is therefore a prerequisite for a trustworthy Gate 1 number,
not a later optimisation.

## Explicitly out of scope for Phase 1

- SIP / telephony (a separate, large project)
- Function calling, RAG, multi-node coordination
- Production observability, recording, cost attribution

## Known risks

- **Rebuilding realtime turn-taking in Go is large.** VAD is solved via
  sherpa-onnx, but LiveKit's semantic EOU turn model is Python-only — porting it
  or running a sidecar is the biggest single risk to the rewrite thesis. Note
  sherpa-onnx does **not** ship a turn detector; Smart Turn v3 and TEN's own
  turn-detection model are separate projects.
- **WebRTC was laggier than WebSocket** in an earlier demo. Validate p95 latency
  early; don't assume.
- **AI-vendor cost, not compute, dominates at 5K.** Track ₹/concurrent-call from
  the first measurement.
- **Clock skew breaks LiveKit auth.** The SFU box has drifted repeatedly, and a
  server clock behind the worker's makes every join token fail `nbf` validation.

# Architecture Decision Records

Short, dated records of choices and *why*, so future-you doesn't relitigate them.

---

<a id="adr-001"></a>
## ADR-001 — LiveKit: official `server-sdk-go` over `am-sokolov/livekit-agent-sdk-go`

**Status:** accepted (Phase 1)

**Context.** Two ways to build a Go worker against LiveKit:
- `am-sokolov/livekit-agent-sdk-go` — community SDK implementing LiveKit's agent
  *dispatch* protocol (worker registration, job assignment). Named in the HLD.
- `livekit/server-sdk-go/v2` — official SDK to join rooms as a participant. No
  job dispatch, but officially maintained.

**Decision.** Use the **official `server-sdk-go`** and join rooms manually for
Phase 1.

**Why.** The Phase 1 question is per-call **density and latency**, which does not
need job dispatch. Betting the core worker on a single-maintainer community SDK
adds supply-chain and protocol-drift risk before we've validated anything. Adopt
dispatch in Phase 2 only if the density case passes — at which point `am-sokolov`
(or building dispatch on the official SDK) is re-evaluated with real data.

**Revisit when:** entering Phase 2 (need a worker pool / autoscaling).

---

<a id="adr-002"></a>
## ADR-002 — Providers: Sarvam STT + Gemini LLM + Sarvam TTS

**Status:** accepted (Phase 1)

**Context.** The HLD named Deepgram/OpenAI/Cartesia (English-optimized). The real
product is Indian-language voice, already running on Sarvam.

**Decision.** Phase 1 uses **Sarvam STT (`saaras:v3`)**, **Gemini
(`gemini-2.5-flash`)**, **Sarvam TTS (`bulbul:v2`)**.

**Why.** Measuring with the providers we actually run makes both **density and
latency numbers transfer to production**. Testing on English-only providers would
give latency/quality figures that don't reflect the Hindi workload. Providers sit
behind `stt`/`llm`/`tts` interfaces so a second provider can be A/B-tested.

**Revisit when:** cost modeling at scale suggests self-hosted GPU inference.

---

<a id="adr-003"></a>
## ADR-003 — Module path `github.com/go2market/go-agent-worker`

**Status:** provisional

**Context.** The Go module path drives every import and must match the eventual
git repo.

**Decision.** Default to `github.com/go2market/go-agent-worker`.

**Why / caveat.** Placeholder pending confirmation of the real org/repo. If it
changes, update `go.mod` and re-run `go mod tidy` before real code lands (cheap
now, disruptive later).

**Revisit when:** the actual repo is created.

---

<a id="adr-004"></a>
## ADR-004 — Phase 1 is a measurement spike, not production

**Status:** accepted

**Context.** It's tempting to treat a working demo as "ready" (as happened with
the SFU benchmark, which proved media but was read as proving the whole system).

**Decision.** Phase 1 code is a **measurement vehicle**. It may be thrown away.
Its success criterion is *producing the numbers in [METRICS.md](METRICS.md)*, and
Decision Gate 1 is allowed to **kill the rewrite** if Go doesn't clearly beat a
Pipecat baseline.

**Why.** Prevents sunk-cost momentum from carrying an unjustified rewrite into
Phase 2/3.

---

<a id="adr-005"></a>
## ADR-005 — Build requires CGO + libopus (Linux target)

**Status:** accepted (forced by dependencies)

**Context.** LiveKit's `server-sdk-go/pkg/media` PCM tracks depend on
`media-sdk/opus`, which wraps `gopkg.in/hraban/opus.v2` — a **CGO** binding to
the system **libopus**. Any package importing the LiveKit media path
(`internal/livekit`, `internal/agent`, `cmd/agent`) therefore needs
`CGO_ENABLED=1` and libopus installed at build time.

**Decision.** Build and run the worker on **Linux amd64 with CGO enabled and
libopus-dev installed**. The pure-Go packages (`config`, `internal/stt`,
`internal/tts`, `internal/llm`) build anywhere.

**Consequences.**
- The Windows dev box (32-bit, CGO off) can build/verify only the pure-Go
  packages. The media/agent/main packages must be built on Linux (see SETUP.md).
- Do **not** run the real worker on 32-bit Go — audio concurrency will exceed the
  ~2 GB address space.

**Revisit when:** a pure-Go Opus codec becomes viable, or media handling moves
out-of-process.

---

<a id="adr-006"></a>
## ADR-006 — Ops HTTP via stdlib net/http, no web framework

**Status:** accepted

**Context.** The worker needs only `/healthz` and `/metrics`. A web framework
(Gin/Chi) would be dependency and abstraction overhead for 2–3 handlers on a
realtime binary whose per-call profiling we want kept clean.

**Decision.** Use the standard library `net/http` for ops endpoints. Any router
(Chi per the HLD) belongs in the separate control-plane API, not this worker.

**Revisit when:** the worker grows a real HTTP API surface (it shouldn't).

---

<a id="adr-007"></a>
## ADR-007 — Docker is the primary build and run path

**Status:** accepted

**Context.** [ADR-005](#adr-005) forces CGO + Linux for the media path, which the
Windows dev box cannot satisfy. Developers still need to run the whole agent.

**Decision.** Ship a multi-stage [Dockerfile](../Dockerfile) and
[docker-compose.yml](../docker-compose.yml). Builder stage
(`golang:1.26-bookworm`) installs `build-essential`, `pkg-config`, `libopus-dev`,
`libopusfile-dev` and **`libsoxr-dev`**; the runtime stage
(`debian:bookworm-slim`) carries only `libopus0`, `libopusfile0`, `libsoxr0` and
`ca-certificates`.

**Why `libsoxr-dev`.** `livekit/media-sdk` pkg-configs against `soxr` for
resampling. It was missing from the original SETUP instructions, so the
documented build never actually worked.

**Networking.** Compose uses `network_mode: host`. The worker is itself a WebRTC
participant doing its own ICE/RTP, so bridged Docker networking breaks NAT
traversal for the UDP media path.

**Revisit when:** media handling moves out-of-process, removing the CGO
constraint.

---

<a id="adr-008"></a>
## ADR-008 — Sarvam TTS requires `send_completion_event=true`

**Status:** accepted (forced by the provider)

**Context.** TTS never produced a single audible byte. The socket connected, the
`config`/`text`/`flush` messages were all accepted, and then nothing came back —
no audio, no error. The provider dashboard confirmed **₹0 of TTS spend across an
entire day of testing**, i.e. synthesis had never once run.

**Decision.** Always append `&send_completion_event=true` to the TTS WebSocket
URL.

**Why.** Verified by direct probe with everything else held constant:

| Config | Result |
| --- | --- |
| `bulbul:v3` + `shubh`, no flag | timeout, 0 samples |
| `bulbul:v3` + `shubh`, **with flag** | 4,096 PCM samples |
| `bulbul:v2` + `anushka`, with flag | 10,752 PCM samples |

Without the flag Sarvam silently emits nothing. `pipecat`'s Sarvam client sets it
too. Note the earlier hypothesis — that speaker `anushka` was invalid for
`bulbul:v3` — was **never demonstrated** and is not the cause.

---

<a id="adr-009"></a>
## ADR-009 — STT emits one ordered event stream

**Status:** accepted

**Context.** `internal/stt` originally exposed transcripts, `START_SPEECH` and
`END_SPEECH` on three separate channels. `Pipeline.Run` selected over them.

**Decision.** Emit a single ordered `Events()` channel carrying
`EventSpeechStarted` / `EventSpeechStopped` / `EventTranscript`.

**Why.** Go's `select` chooses uniformly at random among ready cases, so two
channels destroy the ordering Sarvam sent. A `START_SPEECH` processed *after* its
own transcript cancels the turn that transcript just started — and once one event
behind, every later turn is killed by the previous utterance's signal. The agent
goes permanently silent after 2–3 exchanges. `livekit/agents` routes VAD signals
and transcripts through one ordered handler explicitly to preserve "temporal
consistency between speech detection and recognition results."

---

<a id="adr-010"></a>
## ADR-010 — No keepalive on the Sarvam STT socket

**Status:** accepted (forced by the provider)

**Context.** A 5s `{"type":"ping"}` keepalive was added to the STT client,
mirroring the TTS protocol, to protect idle sockets.

**Decision.** Send **no** keepalive on the STT socket.

**Why.** Probed directly against the API. Sarvam STT rejects any message lacking
an `audio` field:

```
recv type="error" msg="Error in Pipeline : Invalid request: 'audio' must not be None."
CLOSED: websocket: close 1000 (normal)
```

The ping did not preserve the connection — it **destroyed** it, once every 5
seconds of silence. An untouched idle socket survived **75s+** in the same probe.
The keepalive was pure harm.

**Related finding.** Sarvam reports STT failures in `data.message`; our client
read only `data.error`, so this error (and every other STT error) was invisible
in the logs. Both fields are now logged.

---

<a id="adr-011"></a>
## ADR-011 — Lazy reconnect, never a background retry loop

**Status:** accepted

**Context.** A first attempt at reconnection ran a standing retry loop in the
receive path, redialing with backoff whenever a socket dropped.

**Decision.** Each connection owns its read loop and simply exits when that
connection dies. Redial happens **on the next send**: synchronously in
`tts.Speak`, asynchronously in `stt.SendPCM` (which runs on the LiveKit decode
goroutine and must never block).

**Why.** A standing loop retries whether or not there is work to do, so a
permanently rejected config becomes a sustained ~4 dials/min forever. That
exhausted the Sarvam rate limit during debugging and kept it exhausted, masking
the real error underneath. `pipecat` uses the same lazy model: check socket state
at send time, reconnect if closed. An idle broken config now costs nothing.

---

<a id="adr-012"></a>
## ADR-012 — The local energy gate is disabled by default

**Status:** superseded by [ADR-013](#adr-013) — the gate was deleted when Silero
landed. Retained because the failure it records is the reason we do **not** gate
the STT stream.

**Context.** `internal/audio.Gate` is an RMS-energy gate added to keep background
conversation out of the transcript stream and to stop paying Sarvam to listen to
silence. Enabling it made things markedly worse: whole utterances went
untranscribed and STT sockets began dropping.

**Decision.** Ship the gate behind `VAD_ENABLED`, defaulting to **false**. Audio
streams to Sarvam continuously.

**Why.** Two independent problems:

1. **Dropping chunks corrupts a streaming ASR.** Sarvam's recogniser and its
   endpointing assume a continuous stream. Splicing out "quiet" audio clips word
   onsets, removes the silence its endpointing reasons about, and garbles
   transcripts.
2. **RMS cannot tell speech from noise.** It measures loudness only. It worked at
   all against background *voices* purely because distant speakers are quieter —
   and browser AGC, which normalises levels, erodes even that.

The right shape is Silero VAD, which emits buffered speech **segments** (with
`prefix_padding_duration` and `min_silence_duration`) rather than punching holes
in a live socket. Note the gate's parameters already mirror Silero's — the
missing piece is a trained speech model instead of an amplitude threshold.

**Revisit when:** Silero-over-ONNX lands; this gate is then replaced, not tuned.

---

<a id="adr-013"></a>
## ADR-013 — Silero VAD via `yalue/onnxruntime_go` (general binding, not a Silero wrapper)

**Status:** superseded by [ADR-018](#adr-018) — Silero, `yalue/onnxruntime_go`,
`cmd/onnxprobe` and the pinned onnxruntime image stage were all replaced by
sherpa-onnx. The reasoning below is kept because its *central argument was
correct and is what ADR-018 acts on*: composability was the thing that mattered,
and the second model did arrive. It just arrived as GTCRN rather than a turn
detector, and the general binding that best served it turned out to be a
higher-level one.

**Context.** Local VAD is needed so barge-in stops depending on Sarvam's
server-side `START_SPEECH`, which fires on background conversation and lets room
noise hijack a turn. `livekit/agents` uses **Silero VAD** (ONNX, local CPU) for
exactly this — not WebRTC VAD. Two Go routes existed:

- `streamer45/silero-vad-go` — a purpose-built Silero wrapper. Near-zero code for
  us; exports only `Detector`/`SpeechConfig`/`Segment`; pins onnxruntime
  **1.18.1**.
- `yalue/onnxruntime_go` — a general ONNX Runtime binding. Arbitrary models,
  dynamic shapes, concurrent sessions; targets C API **1.28**.

**Decision.** Use **`yalue/onnxruntime_go`**, and write Silero's tensor plumbing
ourselves in `internal/audio`.

**Why.** The deciding factor was composability, not convenience. A Silero-only
wrapper cannot run any *other* ONNX model, so the moment a turn-detector or any
second model is wanted, a general binding gets added alongside it — leaving
**two CGO bindings over libonnxruntime in one process**, pinned to *different C
API versions* (1.18.1 vs 1.28), both calling `InitializeEnvironment`. Choosing
the general binding once avoids that entirely. The cost is ~150 lines of tensor
code we own.

**Consequences.**
- `internal/audio` is **no longer pure Go** — it needs CGO, so it cannot be
  built or tested on the Windows dev box. Run tests in the Docker build stage.
- The runtime image must carry `libonnxruntime.so`, and **its version must match
  the C API version the binding requests**. Mismatch fails at startup with
  `The requested API version [N] is not available`. Pinned via the
  `ONNXRUNTIME_VERSION` build arg; bump it with the Go dependency.
- `silero_vad.onnx` (~2.3 MB, MIT) is vendored under `models/` so builds are
  reproducible and work offline.

**Verification.** The Silero tensor contract is documented nowhere
authoritative, so `cmd/onnxprobe` was written to confirm it against the vendored
model rather than assume it:

```
INPUTS:   input [-1 -1] float32   state [2 -1 128] float32   sr [] int64
OUTPUTS:  output [-1 1] float32   stateN [-1 -1 -1] float32
```

The probe also asserts the model's output *varies with input* — a mis-wired
audio tensor still returns plausible numbers, just identical ones. Re-run it
after any model or onnxruntime upgrade.

**Known limitation.** Only synthetic audio has been run through it. Silence is
correctly rejected and the output responds to input, but **no verification with
real human speech has happened yet** — synthetic signals all score far below
threshold, as a speech-trained model should make them. First live call is the
real test.

---

<a id="adr-014"></a>
## ADR-014 — Local VAD drives barge-in, but does not gate the STT stream

**Status:** accepted

**Context.** With Silero available, the tempting next step is to send Sarvam only
the speech segments and stop paying to stream silence (~₹0.50/min/call).
[ADR-012](#adr-012) records why that backfired.

**Decision.** Silero decides **interruptions only**. Every audio frame still goes
to Sarvam on a continuous socket. Sarvam's `END_SPEECH` still ends a caller turn;
its `START_SPEECH` is ignored whenever local VAD is active.

**Why.** Gating a streaming ASR mid-connection corrupts it: word onsets are
clipped, the silence its endpointing reasons about is deleted, and utterances go
missing entirely. Real cost reduction needs the session torn down between
utterances — which trades reconnect latency at the start of every turn — and
that is a separate decision from getting interruptions right.

**Consequences.**
- Interruption quality improves and no longer depends on a network round trip.
- **The STT bill does not change.** Silero here is a quality and
  benchmark-integrity feature, not a cost one.
- Per-call CPU now includes real local inference (~1 inference per 32 ms of
  audio), which is what makes the density metric comparable to a Pipecat or
  `livekit/agents` worker. See [METRICS.md](METRICS.md).
- If local VAD fails to load, the pipeline logs it and falls back to Sarvam's
  VAD rather than refusing to start.

**Revisit when:** cost forces per-utterance STT sessions.

---

<a id="adr-015"></a>
## ADR-015 — TTS audio is filtered by generation at playback, not stopped at the source

**Status:** accepted

**Context.** With barge-in working ([ADR-013](#adr-013), [ADR-014](#adr-014)), a
call showed a real audio-quality bug: interrupting turn 5 only 72ms into its
reply, then a fast follow-up turn 6, produced a `metrics: turn=6` line closing
**31ms** after the turn started, with no `llm_ttft_ms`/`tts_ttfb_ms` — meaning
`FirstAudioOut()` fired before turn 6 had called the LLM. `playbackLoop` had
written stray audio from the *cancelled* turn 5 into the room, misattributed to
turn 6, because it was the first thing to arrive after playback re-opened.

**Root cause.** Sarvam's TTS connection is call-scoped and reused across turns
([ADR-011](#adr-011)), and there is no "stop synthesizing" message in its
protocol. Cancelling the turn's Go context stops *us* from sending more text,
but Sarvam keeps streaming audio for whatever was already requested. `onBargeIn`
only drained the local channel once, at interrupt time — anything Sarvam sent
afterward slipped through and got played as if it belonged to the next turn.

**Investigated and rejected: closing/reconnecting the TTS socket on barge-in.**
Fully closes the hole, but LiveKit Agents' own `SpeechHandle` — the reference
implementation this project measures itself against — deliberately does **not**
do this. Its `interrupt()` cancels the asyncio task pulling audio from TTS and
tags each speech with a generation, checked right before playback; it never
reconnects the provider. The reasoning: a reconnect adds latency to the *next*
reply, which is the reply the caller is now waiting for — the worst possible
moment to add delay.

**Decision.** Match that design. Two additions:

1. **`internal/tts`**: `Speak(text, gen)` takes the caller's generation number.
   Each decoded `Chunk` is tagged with it, using Sarvam's `{"event_type":"final"}`
   message (sent because `send_completion_event=true`, [ADR-008](#adr-008)) as
   the boundary between one request's audio and the next — a FIFO queue of
   pending generations, pushed in `Speak`, popped on `final`.
2. **`internal/agent`**: `Pipeline.curGen` holds the only generation
   `playbackLoop` will write to the room. Set on every `startTurn`; cleared to 0
   on every `onBargeIn`. The check happens **per chunk, at the moment of
   playback** — not once at interrupt time — so audio arriving any amount of
   time after the interruption is still caught.

**Why not `request_id`.** Sarvam's audio frames do carry a `request_id`, which
looked like a cleaner correlation key than a "final"-event queue. Verified
directly against the live API with two sequential `text`+`flush` requests on one
connection: `request_id` stayed **identical** across both — it identifies the
session, not the request. Using it would have silently failed to distinguish
sentence 1's audio from sentence 2's. The two `final` events, by contrast,
arrived in the correct order matching the two requests. Recorded here so the
`request_id` field is not re-tried later on the same mistaken assumption.

**Consequences.**
- A connection drop or reconnect ([ADR-011](#adr-011)) clears the pending-gen
  queue: whatever `final` events the old connection owed can never arrive on
  the new one.
- `drainAudio()` is now a courtesy (frees channel capacity promptly), not a
  correctness requirement — the generation check is what actually closes the
  leak, regardless of timing.
- Ordering across requests relies on Sarvam preserving request order on one
  WebSocket, which was verified for two consecutive requests but not
  stress-tested (e.g. against very rapid interrupt/re-speak cycles).

**Confirmed by the diagnostic logging, in the very next live call.** Two
occurrences in one ~5-minute session:

```
sarvam tts: gen=4 final received after 5.09s (1 generation(s) still queued)
agent: resumed gen=5 after dropping 115 stale gen=4 chunk(s) over 4.86s

sarvam tts: gen=6 final received after 4.75s (1 generation(s) still queued)
agent: resumed gen=7 after dropping 62 stale gen=6 chunk(s) over 2.55s
```

`final` delays climbed across the call — 2.7s, 3.5s, 5.1s, 4.75s, 7.1s, up to
**9.5s** — while the *audio itself* for the next reply was already flowing.
177 real chunks across two replies were dropped as "stale," which means both
replies very likely started playback with their opening words missing, not
merely delayed. This is worse than the delay `tts_ttfb_ms` alone suggested.
Fixed in [ADR-017](#adr-017).

---

<a id="adr-016"></a>
## ADR-016 — Speech-end latency reference is backfilled, not read at turn start

**Status:** superseded by [ADR-017](#adr-017). The backfill mechanism this ADR
introduced had its own bug — a genuinely different, later utterance's
speech-end could be backfilled onto an earlier turn, producing negative
latency — found in the very next live call. Kept here because the two
consecutive failures (stale-value-from-the-past, then
wrong-value-from-the-future) are what motivated finding an authoritative
source instead of continuing to patch the correlation logic.

**Context.** A live call's `metrics: turn=2` line reported `total_ms=23912` —
23 seconds. The reply itself was fast (`tts_ttfb_ms=327`); the number was
fiction. Reconstructed from the same call's raw timestamps:

```
12:42:33.531  stt: END_SPEECH        ← the caller actually stopped here
12:42:33.750  stt: TRANSCRIPT "..."  ← 219ms later, normal
12:42:33.750  agent: turn 2 start
12:42:34.114  vad: SPEECH_END        ← Silero's own end-of-speech, AFTER the transcript
```

**Root cause.** `lastSpeechEnd` was one shared value, stamped whenever a
`SPEECH_END`/`END_SPEECH` fired, with no link to *which* utterance it belonged
to. `startTurn` read it whenever a transcript arrived, on the assumption that
the matching speech-end had already landed. Usually Silero leads Sarvam's
transcript by ~200ms and that assumption holds — but Silero and Sarvam are two
independent, differently-paced pipelines, and nothing guarantees the ordering.
This time Sarvam's transcript won the race, so `startTurn` read `lastSpeechEnd`
before turn 2's own speech-end had been recorded — and got turn 1's value from
22 seconds earlier instead.

**Decision.** Track whether the caller is currently mid-utterance
(`speechActive`, true between `SPEECH_START` and `SPEECH_END`). If a transcript
starts a turn while it is still true, the real speech-end for that utterance
hasn't arrived — leave the turn's reference zero and **backfill it** the moment
the real `SPEECH_END` does fire (`metrics.Turn.SetSpeechEnd`), rather than
substituting a stale value.

`SetSpeechEnd` is a no-op if the turn already has a value, or is already
closed — if `FirstAudioOut` printed the metrics line before the real speech-end
showed up, silently attaching a number after the fact would be worse than
omitting it: a right-looking number nobody can tell apart from a correct one.

**Consequences.**
- The rare case (turn starts before its own speech-end is known) now reports
  either a correct, backfilled figure or no `endpoint_ms`/`total_ms` at all —
  never a figure borrowed from a different utterance.
- This shares no mechanism with [ADR-015](#adr-015)'s generation tagging, but
  is the same underlying lesson applied a second time: two independently-paced
  async pipelines (Silero/Sarvam here; Sarvam TTS chunks/`final` events there)
  racing each other is not safe to resolve with "whichever value is currently
  sitting in a shared variable."

---

<a id="adr-017"></a>
## ADR-017 — Reference-implementation audit: authoritative timestamps over correlation, conditional reconnect over pure filtering

**Status:** accepted

**Context.** [ADR-015](#adr-015)'s generation-tagging theory was confirmed live
(177 real chunks dropped across two replies in one call — see the "Confirmed"
note there), and [ADR-016](#adr-016)'s backfill mechanism produced a *second*
bug (negative latency) one call after fixing the first. Both were attempts to
correlate two independently-timed signals after the fact. Before patching
either again, `livekit-agents`, `pipecat`, and `TEN` were checked for how the
reference implementations actually handle this. (TEN's source wasn't
reachable — no `ten_ai_base` docs, code search needs a GitHub login this
session doesn't have, and guessed file paths 404'd. Not counted as evidence
either way.)

**Finding 1 — LiveKit Agents does reconnect; an earlier read of it was wrong.**
[ADR-015](#adr-015) said their `SpeechHandle.interrupt()` "never reconnects,"
based on seeing it cancel an asyncio task. That was incomplete: the task being
cancelled is what's reading a `SynthesizeStream`, and that stream's `aclose()`
"closes the underlying connection and cancels pending tasks... actively
terminates the connection." Their documented pattern goes further —
`SynthesizeStream.push_text()` warns that reusing one instance "across multiple
segments is deprecated; create a new SynthesizeStream instance for each
segment." Every sentence gets its own connection lifecycle, not just
interrupted ones. There is no multiplexed-shared-connection problem to solve
in their design because they don't multiplex.

**Finding 2 — pipecat's policy is conditional, and matches a check we already
make.** pipecat's base `TTSService` keeps `context_id` tagging as an always-on
filter (equivalent to our generation tagging) but its `WebsocketTTSService`
subclasses — the same protocol shape as Sarvam's — additionally reconnect:

```python
should_reconnect = self._bot_speaking or self._tts_started
```

Only reconnect if synthesis was genuinely in progress. `onBargeIn`'s first line
is already `if !p.isSpeaking() { return }` — every barge-in that reaches the
reconnect call already satisfies pipecat's condition, for a different original
reason (deciding whether to act on the interruption at all).

**Finding 3 — LiveKit's STT timing doesn't correlate two clocks; it reads one
the provider stamps.** `SpeechData.end_time`, extracted directly from the STT
provider's own response (their Speechmatics plugin is the documented example),
combined with a stream-start anchor, computes the acoustic stop time — no
comparison against a separately-clocked local VAD signal. This is exactly the
class of correlation that broke twice in [ADR-016](#adr-016).

**Decision.**

1. **TTS ([internal/tts](../internal/tts/sarvam.go)):** add `Client.Disconnect`,
   closing the current connection so the next `Speak` redials on a clean
   session (empty generation queue). Called from `onBargeIn` only past the
   `isSpeaking()` check — i.e. only when pipecat's condition is already true.
   Generation tagging ([ADR-015](#adr-015)) stays as the always-on safety net;
   this closes the specific gap a slow `final` event left open.
2. **STT ([internal/stt](../internal/stt/sarvam.go)):** before building a
   utterance-correlation queue to fix [ADR-016](#adr-016) properly, probed
   Sarvam's actual STT payload for a provider-side timestamp — same
   verify-before-build approach that already caught the TTS `request_id`
   mistake. Found one: `START_SPEECH`/`END_SPEECH` events carry `occured_at`, a
   fractional Unix timestamp from Sarvam's own clock, undocumented and
   previously unparsed:
   ```
   {"type":"events","data":{"signal_type":"END_SPEECH","occured_at":1786013420.5537887}}
   ```
   Sarvam delivers `START_SPEECH → END_SPEECH → TRANSCRIPT` in strict order on
   one connection, so its own `END_SPEECH` timestamp is guaranteed to belong to
   the transcript that follows it — no correlation against Silero's
   independently-paced clock needed at all. `lastSarvamSpeechEnd` replaces
   [ADR-016](#adr-016)'s `speechActive`/`pendingSpeechEndTurn`
   backfill mechanism outright; `metrics.Turn.SetSpeechEnd` and its tests were
   deleted as dead code, not deprecated in place.

   > **Amended.** Using `occured_at` as the timestamp was wrong — the clocks are
   > ~3.87s apart and it is a detection time, not an acoustic speech-end. The
   > *ordering* argument in this paragraph is correct and still in force; only
   > the clock changed. See the amendment at the end of this ADR.

**Consequences.**
- Local VAD remains the barge-in trigger (still faster: no network round trip)
  but no longer feeds the metrics speech-end reference at all — that is
  Sarvam's `END_SPEECH`, unconditionally, once local VAD is proven or not. Two
  previously conflated concerns (which source triggers interruption, which
  source is trustworthy for latency math) are now fully separate.
- The reconnect costs one redial on the turn immediately following a genuine
  mid-speech interruption, not on every barge-in — turns interrupted before
  speaking, or barge-ins with nothing active, cost nothing extra. **Measured in
  the next live call:** the turn after an interruption had `tts_ttfb_ms=1115`
  against 263–579ms on every other turn. So ~600ms, once, on that turn only.

### Amendment — the clock assumption was wrong (confirmed in a live call)

The "unverified" risk logged here was **the** flaw, and it invalidated the
speech-end half of this ADR. Recording it in place rather than as a new ADR,
because the decision above is what produced it.

The risk as written was: *"whether Sarvam's server clock and this process's
clock agree closely enough for `occured_at` to be safe to mix with local
`time.Now()` calls in the same latency figure … nothing currently logs the
gap."* Both halves came true — the clocks did not agree, and because nothing
logged the gap, the resulting numbers looked plausible for a whole call.

**Symptom.** `endpoint_ms` sat at ~4000ms on all nine turns while its own
components swung freely (`llm_ttft_ms` 614→1438, `tts_ttfb_ms` 263→1115). A
figure containing those cannot be that flat.

**Diagnosis.** Reconstructing the implied `speechEnd` per turn from
`endpoint_ms = transcript − speechEnd` and comparing it to local receipt of
`END_SPEECH`:

```
turn      2      3      4      5      6      7      8      9     10
offset  3.890  3.857  3.875  3.863  3.892  3.878  3.859  3.872  3.878   (seconds)
```

3.857–3.892s — a 35ms spread across 90 seconds. Detection lag varies with what
the caller says; a clock offset does not. Every speech-end-anchored figure was
inflated by ~3.87s:

| | reported | actual (local clock only) |
| --- | --- | --- |
| `endpoint_ms` (speech-end → transcript) | ~4000ms | **~130ms** |
| `total_ms` (speech-end → first audio) | 5203–5981ms | **1313–2104ms**, mean ~1640ms |

`agent_response_latency_seconds` — the Decision Gate 1 number — was reporting
~5.5s for something that is really ~1.6s, and saturating `latencyBuckets`
(top bucket 5.0) so every sample landed in `+Inf`.

**The premise was also wrong, independently of the skew.** This ADR assumed
`occured_at` was an authoritative *acoustic* speech-end. Checked against local
VAD on turn 4: `SPEECH_END` at `24.277` less the 550ms hangover puts true speech
end near `23.727`, while skew-corrected `occured_at` lands at `24.125` — ~400ms
late, and within ~35ms of when the event was received. `occured_at` is Sarvam's
*detection* timestamp, effectively "when I sent this". It was never worth
crossing clocks for.

**Amended decision.** `lastSarvamSpeechEnd` is now the **local receipt time** of
`END_SPEECH`. `Event.At` is local; Sarvam's value moved to a separate
`Event.RemoteAt` so latency arithmetic cannot reach for the wrong clock by
accident. `Pipeline.reportClockSkew` logs the gap once per call above a 250ms
threshold.

**What survives unchanged is the ordering argument**, which was the actual fix
for [ADR-016](#adr-016): `END_SPEECH` always precedes its own transcript on one
ordered connection processed by one goroutine, so there is nothing to correlate.
That was right. Only the choice of clock was wrong.

**The lesson worth keeping:** the diagnostic that would have caught this in ten
seconds was named in this very ADR and not built, because it was classified as a
follow-up rather than part of the change. A risk identified precisely enough to
write down is worth the ten lines that make it visible — writing it down is not
the same as handling it.

**Follow-on bug from this same amendment, caught in the next live call.**
`metrics.Turn` also reported `total_adj_ms`, subtracting `VAD_HANGOVER_MS` from
the raw total on the theory that SPEECH_END fires a hangover after the caller
actually stopped talking. That was true when `speechEnd` came from the *local*
VAD, whose hangover really did delay it. Once `speechEnd` moved to Sarvam's own
`END_SPEECH` above, the subtraction kept running against the wrong quantity —
Sarvam's endpointing delay has nothing to do with our local hangover setting —
and silently discounted the reported latency by ~550ms on every turn. Confirmed
in a live call: `total_adj_ms=976` on a turn whose caller-perceived latency,
cross-checked independently, was over 2 seconds.

Removed rather than recomputed. `endpointLag` (Sarvam's own speech-end-to-
transcript lag) is still reported on its own, so a slow turn is still visible —
just not laundered into a "corrected" figure. No replacement adjustment was
added, because the only inputs available for one are either the removed local
hangover (proven wrong here) or a fresh correlation against local VAD (the
exact failure mode ADR-016 hit twice). `Pipeline.New` and `metrics.NewTurn`
both dropped the now-unused hangover parameter rather than keeping it as dead
weight.

- Re-confirms the pattern from [ADR-008](#adr-008)/[ADR-010](#adr-010): Sarvam
  has repeatedly sent fields (`request_id`, `final`, now `occured_at`) that
  were sitting in the response the whole time, unread. Worth treating "did we
  actually inspect every field in the raw payload" as a standing check before
  building custom correlation logic against any Sarvam message type.

---

<a id="adr-018"></a>
## ADR-018 — sherpa-onnx for TEN VAD + GTCRN, replacing Silero and the raw ONNX binding

**Status:** accepted, supersedes [ADR-013](#adr-013)

**Context.** Two things were wanted: replace Silero with **TEN VAD** (lower
false-positive rate on non-target audio), and add **GTCRN** speech enhancement
in front of both the VAD and Sarvam STT, with a toggle. The stated motivation
for both was people talking 3–4 metres away triggering the agent.

Two routes, and the choice between them reversed twice on evidence. Recording
why, because both reversals were caused by assumptions that a few minutes of
verification would have caught earlier.

**Reversal 1 — "same pattern as Silero" was wrong.** The initial recommendation
was to keep `yalue/onnxruntime_go` and add both models as further raw ONNX
sessions, on the reasoning that the Silero integration cost was already paid.
That reasoning assumed these models take raw PCM, because Silero does. They do
not:

- **TEN VAD** takes `{1, 3, 41}` — a 3-frame history of 41-dim features — plus
  four `{1, 64}` recurrent state tensors. Producing those features needs
  pre-emphasis, a 768-sample window, a **1024-point RFFT**, a 40-bin Slaney mel
  filterbank, log scaling, and per-bin normalisation.
- **GTCRN** takes `(1, 257, 1, 2)` — a complex STFT frame, not audio. STFT and
  ISTFT both happen *outside* the model (`n_fft=512`, `hop=256`, sqrt-Hann
  window), so enhanced audio only exists after inverse-transforming and
  overlap-adding it yourself.

Go's standard library has no FFT. Route A therefore meant ~400–600 lines of DSP,
not "two more sessions".

**Reversal 2 — the pitch feature.** More decisive: sherpa's TEN VAD **does not
compute the model's pitch feature**, substituting zero. Its own model metadata
says so — *"It uses 0 as the pitch feature, which may degrade the performance."*
The 41st feature's normalisation constants (mean `92.36`, inv_stddev `0.0087`)
are unmistakably Hz-scaled. Measured against the official TEN VAD on its own
test audio, 476 frames:

```
sherpa (pitch=0) vs official TEN VAD:  77.9% agreement
  sherpa speech, official not:  68 frames (14.3%)  <- false triggers
  sherpa not, official speech:  37 frames (7.8%)   <- missed speech
```

The error is asymmetric and points the **wrong way for the stated goal**:
pitch=0 produces ~14% *more* false speech triggers, and fewer false triggers was
the entire reason for wanting TEN VAD. Matching the official implementation
would require TEN's pitch algorithm, which is not published — their pip package
ships a prebuilt `.so`. So **pitch=0 is the only implementation available**,
whichever route is taken.

That collapsed route A's case entirely: it meant writing hundreds of lines of
error-prone DSP to arrive at *exactly the same quality* sherpa gives from a
config struct. Two careful Python reimplementations of sherpa's own C++ reached
only 85.5% and 83.4% agreement with sherpa's decisions — close enough to
demonstrate the frontend is genuinely fiddly, not close enough to port.

**Decision.** Use **`k2-fsa/sherpa-onnx-go`** for both TEN VAD and streaming
GTCRN. Delete `internal/audio/silero.go`, `cmd/onnxprobe`, the
`yalue/onnxruntime_go` dependency, and the Dockerfile's pinned onnxruntime
stage.

**Why.** Identical model quality for a config struct instead of a DSP project;
sherpa drives Silero and TEN VAD through the *same* `VadModelConfig`, so
comparing them becomes a config change rather than a rewrite — which is what
Phase 1 is for. ADR-013's core argument (pick the composable binding, the second
model always comes) still holds; sherpa is simply the more composable choice at
a higher level.

**Verification.** Proven in Docker *before* any repo file was touched, because
the risk was integration, not code:

- `sherpa-onnx-go` and the libopus CGO media path link into one binary
- TEN VAD fires; streaming GTCRN reports frame shift 256 (16 ms) and an energy
  ratio of 0.555 — genuinely suppressing, neither passing through nor zeroing
- runtime image 241 MB, `ldd` resolves every library, no separate onnxruntime

The spike also caught a deployment failure that a compile check would have
missed: `libsherpa-onnx-c-api.so` is **linked, not dlopened**, so the first
bare-runtime attempt died with `cannot open shared object file`. The libs ship
inside the Go module and are copied out with a version glob.

**Consequences.**
- **sherpa bundles its own onnxruntime**, so ADR-013's whole class of problem —
  keeping an image's `libonnxruntime.so` version matched to the binding's C API
  version — disappears along with the `ONNXRUNTIME_VERSION` build arg and
  `ONNXRUNTIME_LIB_PATH`.
- **16 kHz only.** Silero accepted 8 kHz; TEN VAD and GTCRN do not. sherpa
  aborts the *process* from C++ on a rate it cannot serve, so `NewDetector`
  rejects it in Go first.
- **GTCRN carries exactly one frame — 256 samples, 16 ms — of algorithmic
  delay.** Measured, not assumed: the first completed frame returns nothing and
  every frame after returns a full one. It lands on the STT path when
  enhancement is on, and is pinned by a test so a model upgrade that changes it
  fails loudly rather than silently shifting every reported latency.
- **Enhancement defaults off.** It alters what Sarvam hears, and ASR is trained
  on unprocessed audio; [ADR-012](#adr-012) is the cautionary precedent, where a
  local gate corrupted Sarvam's input and cost transcripts.
- **GTCRN will not solve the 3–4-metre problem, and this should not be read as a
  fix for it.** It suppresses noise and is trained to *preserve* speech; a
  distant talker is speech. The literature treats interfering-talker suppression
  as a separate problem needing spatial cues or speaker enrollment, and GTCRN
  appears in that literature as the front-end denoiser feeding a *separate*
  target-speaker-extraction stage. The measures actually aimed at that problem
  are TEN VAD's own false-positive rate and the still-missing `min_words` /
  `min_duration` interruption policy (see ARCHITECTURE.md gap analysis).
- sherpa's VAD buffers every completed speech segment for callers wanting the
  audio. Barge-in reads only the speech/silence edge, so the buffer is drained
  every frame; left alone it grows for the length of the call. Covered by a test
  rather than a comment.
- No turn detector comes with this. sherpa has ASR, TTS, VAD, speaker ID,
  punctuation, enhancement and source separation — but **no** end-of-turn or
  semantic-VAD model. Smart Turn v3 and TEN's own turn-detection model are
  separate projects. The turn-detection gap in ROADMAP.md is unchanged.

---

<a id="adr-019"></a>
## ADR-019 — Gate 1 preconditions: the baseline is Node at 7.07 calls/core, and parity comes before density

**Status:** accepted (Phase 1) — amends [ROADMAP.md](ROADMAP.md) decision gate 1,
supersedes two threshold rows in [METRICS.md](METRICS.md), and closes the revisit
deferred by [ADR-001](#adr-001)

**Context.** Gate 1 was written before there was anything real to compare against.
There now is. The Node/LiveKit stack a Go worker would replace has been benchmarked
to CPU saturation on `.19` (16 vCPU / 31 GB), and this number is trustworthy in a
way the five runs before it were not — each earlier ceiling turned out to be an
artefact (generator CPU starvation, the default `loadThreshold`, file-descriptor
limits, cold-fork `initializeProcessTimeout`, sequential room creation) and each was
ruled out with evidence before the next was tested.

| Measured — 85 concurrent rooms, 0 provider errors | Value |
| --- | --- |
| CPU | 75.2% avg / 90% peak of 16 cores = **12.03 / 14.4 cores** |
| **Density** | **7.07 calls/core** (7.07, 7.3, 7.8, 7.9, 7.3 across five runs) |
| RAM | 21,895 MB = **258 MB/call**, 71% of 31 GB |
| Endpointing delay p95 | 900 ms — `minDelay: 900`, a deliberate tuning choice |
| LLM TTFT p95 | 1,264 ms |
| TTS TTFB p95 | 263 ms |
| Composite speech-end → first audio p95 | ~2,427 ms |

LiveKit's own published sizing is ~7.9 jobs/core, so 7.07 is not a
misconfiguration waiting to be tuned away — it is what the framework costs, while
doing real STT/LLM/TTS rather than their sine-wave reference.

Gate 1 as written has three defects that would each produce the wrong decision.

**1. The pass condition names the wrong competitor.** It asks for "Go density ≥ ~2×
a Pipecat baseline" — Pipecat/Dograh at 5-6 calls/core, so the bar reads 10-12. But
Pipecat is no longer the alternative to a Go rewrite; **Node at 7.07 is.** A rewrite
has to beat the best available option, not the one being retired. Against the
Pipecat bar, Go could land at 11 calls/core, formally "pass", and deliver 1.5× the
stack it actually replaces — nowhere near enough to justify owning a framework.

**2. METRICS.md's placeholder thresholds invert the verdict.** They read "Agent
density: target ≥ 30 calls/vCPU, hard fail < 15". Against a measured 7.07 those are
unanchored in both directions: Go at 14 calls/core — a real 2× win and a clear
reason to proceed — registers as a **hard fail**, while the 30 target is over 4×
anything either framework has demonstrated anywhere. The file flags them as
placeholders; left unedited they are worse than having no threshold at all, because
they look authoritative at the moment the decision is made.

**3. Two confounds would inflate Go's density before it is measured.** Both come
from the Go worker not yet doing work the Node baseline pays for:

- **Endpointing.** Node runs `inference.TurnDetector()`, a multimodal end-of-turn
  model, **in-process on the CPU being measured**. The Go worker still delegates
  endpointing to Sarvam (ROADMAP item 1 says so explicitly). Measured today, Go
  wins partly by not running inference Node is charged for.
- **Turn-taking delay.** Roughly 900 ms of Node's 2,427 ms composite p95 is
  `minDelay: 900`, raised from the 500 ms default so Sarvam's final transcript
  lands before the turn commits over 8 kHz audio. "At equal latency" is meaningless
  unless Go commits turns on the same policy — otherwise Go can buy latency with
  accuracy, or density with either.

**Decision.** Gate 1 may not be answered until all three hold.

1. **Parity before density.** The Go worker runs in-process end-of-turn detection
   and the same interruption/endpointing policy as the Node baseline (ROADMAP items
   1-2, including the `min_words` / `min_duration` policy still open from
   [ADR-018](#adr-018)'s consequences). A density number measured without this is
   void, not indicative.
2. **The bar is 2× Node, not 2× Pipecat.** Proceed at **≥ 14 calls/core** at
   composite p95 ≤ 2,427 ms with 0 provider errors. **Stop at ≤ 10.** Between 11
   and 13, re-cost the project against the memory saving alone (below) rather than
   reading it as a pass.
3. **pprof first, not last.** ROADMAP item 3 (per-stage instrumentation) runs
   **before** items 2 and 4, on the single-call path. A Go CPU profile that splits
   ONNX + libopus — native, irreducible, identical in both languages — from
   orchestration (V8 process overhead in Node, goroutines in Go) bounds the
   achievable density *before* any further code is written.

**Why.** The gate exists to be allowed to kill the project. A gate that can be
passed by measuring a less complete pipeline against a placeholder threshold cannot
do that job. Ordering instrumentation first is the cheapest available refutation: if
per-call CPU turns out to be mostly native inference and codec work, Go's ceiling
sits near Node's, and the gate can be answered in days with a "stop" instead of
weeks with a rewrite. That is the outcome METRICS.md §1 was already written to
detect — *"if per-call CPU is mostly remote I/O, Go barely helps and the density
case collapses"* — and local VAD landing via ADR-018 has only moved the question,
not settled it.

**Consequences.**

- **RAM is a co-binding constraint, and it is the strongest remaining case for Go.**
  METRICS.md §2 anticipated this ("86% RAM vs 71% CPU" on the SFU box) and the Node
  run confirms it: 258 MB/call puts 85 calls at 71% of 31 GB while CPU peaks at 90%.
  Node reaches both walls together, so a larger box buys cores that RAM cannot feed.
  Per-goroutine sessions plausibly reach low double-digit MB. This is a firmer
  argument than the CPU one, which stays unknown until precondition 3.
- **Two METRICS.md threshold rows are superseded and still need editing there:**
  density (30 / 15 → 14 / 10), and p95 latency (≤ 800 ms, which the Node baseline
  does not meet and no measured configuration of either stack has). The 800 ms
  figure should either be restated as a product SLA that the *current* stack also
  fails, or replaced by the parity number above.
- **ADR-001's deferred revisit resolves to "still no".** It left adopting
  `am-sokolov/livekit-agent-sdk-go` to be "re-evaluated with real data" on entering
  Phase 2. The data, as of 2026-09-13: last commit **2025-10-20**, ~11 months stale;
  8 stars, 4 forks. A second candidate raised in review,
  `chriscow/livekit-agents-go`, is self-described prototype/MVP — last commit
  **2025-07-28**, 5 stars, 0 forks, 30 open issues. The supply-chain and
  protocol-drift risk ADR-001 named has since materialised in both. ADR-001's bet
  was correct and stands.
- **Dispatch is cheaper than it looks, which makes precondition 1 the real cost.**
  `livekit/protocol v1.49.0` is already a dependency and already carries every
  dispatch type — `RegisterWorkerRequest`/`Response`, `WorkerMessage`/
  `ServerMessage`, `AvailabilityRequest`/`Response`, `JobAssignment`,
  `JobTermination`, `UpdateJobStatus`, `UpdateWorkerStatus`, `WorkerPing`/`Pong`,
  `MigrateJobRequest`. What is missing is the state machine over them, not the
  protocol, and `agents-js` is a readable specification for it. Turn-taking policy
  is the opposite: open-ended tuning, and the Node figures above took a week of it.
  ROADMAP is right to call item 2 the highest-value remaining item — reviews that
  rank dispatch above it are wrong.
- **Horizontal Node scaling is the fallback and should start regardless.** `.20` sat
  at 5% CPU for the whole benchmark. A second worker there roughly doubles capacity
  with no new code, and it is already the path ROADMAP commits to if this gate
  fails — so taking it now costs nothing and removes the schedule pressure that
  would otherwise push Gate 1 to be answered on a confounded number.

**Revisit when:** precondition 3's profile lands. It either bounds Go's ceiling
below 14 calls/core — answer the gate, stop — or it does not, and preconditions 1-2
become the work.

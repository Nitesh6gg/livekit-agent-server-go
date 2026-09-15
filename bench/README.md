# Ablation harness

Answers [ADR-019](../docs/DECISIONS.md#adr-019) precondition 3, which gates whether
the Go rewrite is worth continuing at all:

> How much of this worker's per-call CPU is native inference and codec work —
> irreducible, identical C libraries in Go and in Node — and how much is
> orchestration, the only part a language change can improve?

Run this **before** turn-taking policy (ROADMAP item 2) or load generation (item 4).
It is the cheapest possible refutation: if native cost dominates, Go's density
ceiling sits near Node's measured 7.07 calls/core and Gate 1 is answered with a
"stop" in days instead of weeks.

## Why not just read a pprof profile

Go's CPU profiler does not unwind C stacks. Time inside `sherpa-onnx` (ONNX
inference for TEN VAD and GTCRN) and `libopus`/`soxr` (decode, encode, resample)
collapses into `runtime.cgocall` instead of breaking down. Both are cgo
([ADR-005](../docs/DECISIONS.md#adr-005), [ADR-018](../docs/DECISIONS.md#adr-018)),
so pprof is blind to exactly the cost that decides the gate — and blind **in Go's
favour**, which is the confound direction ADR-019 exists to prevent.

So the harness measures native cost by **difference**, toggling stages off with the
`VAD_ENABLED` and `GTCRN_ENABLED` config flags that already exist:

| Run | `VAD_ENABLED` | `GTCRN_ENABLED` | Isolates |
|---|---|---|---|
| `idle` | false | false | process floor — sockets, runtime, no call |
| `A` | false | false | orchestration + codec + provider I/O |
| `B` | true | false | A + TEN VAD inference |
| `C` | true | true | B + GTCRN speech enhancement |

`A − idle` is the addressable share. `B − A` and `C − B` are irreducible. The
profile is still captured per run — Go frames in it are trustworthy, so it splits
provider I/O from orchestration *inside* A. Use `perf record -g` against the
container if you need the C side attributed properly.

Everything is reported as **CPU-seconds per call-minute**, never percentages: `.19`
has 16 cores and `.20` has 32, so the same percentage means a different amount of
work on each.

## Running it

You need a caller. This repo has no audio generator — the worker joins a room and
waits — so something must publish real speech into `LIVEKIT_ROOM` for the length of
each run. `CALLER_CMD` has **no default on purpose**: a wrong default produces a run
that looks successful and measures an idle worker four times.

```bash
# verify the flags for your CLI version first
lk room join --help

# the room is a POSITIONAL argument, not --room. `--room agent-spike-1` makes lk
# connect to the wrong room and publish where nothing is listening.
export CALLER_CMD='lk room join --identity bench-caller \
    --publish /root/go-agent-worker/bench/caller.ogg agent-spike-1'

bash bench/ablate.sh
```

The audio must be **real speech**, the **same file at the same length every run**,
and long enough to cover `DURATION` without ending early. Silence under-reports CPU
badly ([METRICS.md](../docs/METRICS.md)), and a tone never triggers endpointing, so
no LLM or TTS turn ever runs — you would measure a pipeline that never did its job.

| Variable | Default | Notes |
|---|---|---|
| `CALLER_CMD` | *(required)* | publishes speech; runs until killed |
| `RUNNER` | `compose` | or `binary` (uses `$BINARY`, default `./bin/agent`) |
| `DURATION` | `180` | seconds of live call per run |
| `SETTLE` | `20` | seconds after `/healthz` before the t0 reading |
| `COOLDOWN` | `20` | seconds between runs |
| `RUNS` | `idle A B C` | subset allowed, e.g. `RUNS="idle A B"` |
| `PPROF_SECONDS` | `60` | `0` skips profile capture |
| `OPS_URL` | `http://127.0.0.1:8080` | must match `HTTP_ADDR` |

Writes `bench/results/<run-id>/` containing `ablation.csv`, `summary.md`, per-run
sample CSVs, caller logs, the agent's own log (`<run>-agent.log`, captured before the
container is destroyed), and `*-cpu.pprof` / `*-heap.pprof`.

**The ramp aborts after any call run that produced zero transcripts**, rather than
continuing for another ten minutes. That failure is not hypothetical: the first real
run of this harness measured an idle worker four times because `CALLER_CMD` pointed at
a path that did not exist, and `lk` exited without publishing — a mistake visible only
in `<run>-caller.log` as `stat ...: no such file or directory` right after
`connected to room`.

**Use an absolute path in `CALLER_CMD`.** It is evaluated from the repo root, so a
relative path that works when you type it by hand in `bench/` will not resolve here.

To read a captured profile:

```bash
go tool pprof -http=:9000 bench/results/<run-id>/C-cpu.pprof

# or, with no Go toolchain on the box:
docker run --rm -v "$PWD:/w" -w /w golang:1.26-bookworm \
  go tool pprof -top -nodecount=30 /w/idle-cpu.pprof
```

## Reading the result

`summary.md` prints the derived split and an implied density. Check four things
before believing any of it:

1. **The `transcripts` column.** Zero on a call run now aborts the ramp outright. But
   also compare the counts *across* A, B and C: the subtraction is only valid if they
   did the same amount of work. A run with noticeably fewer turns synthesized less
   speech and called the LLM fewer times, so part of its apparent "saving" is work
   that simply never happened.
2. **The SUSPECT warning.** If run `A` barely exceeded the idle floor, the caller
   published little or nothing and the rows are suspect. A live call runs STT
   streaming, an LLM turn and TTS synthesis; it cannot cost almost nothing.
3. **The restart column.** A non-zero change means the worker crashed mid-run,
   resetting `process_cpu_seconds_total` — that row is void too. The harness pins
   `restart: "no"` to make crashes visible rather than self-healing.
4. **Pinned models.** `config/config.go` defaults differ from `.env.example`
   (`saaras:v4` vs `saaras:v3`, `bulbul:v2`/`anushka` vs `bulbul:v3`/`shubh`,
   `gemini-2.5-flash` vs `gemini-3.1-flash-lite`). Whichever wins, it must be the
   **same across all four runs** — a different TTS model is a different amount of
   work. Confirm from the worker log.

Then: **the addressable share bounds what a Go rewrite can win on CPU.** It does not
bound the memory win, which is separate and larger — Node costs 258 MB/call and
goroutine-per-session plausibly reaches low double digits.

## Node baseline (`bench/node-baseline.sh`)

Go's N=1 ablation number has nothing honest to compare against on its own — the
only Node figure on record is 7.07 calls/core from 85 concurrent calls at
saturation, where fixed per-process cost amortizes across a loaded box. This
script measures Node the same way `bench/ablate.sh` measured Go: one process, one
call, the same `caller.ogg`.

**Node forks a separate OS process per job.** Sampling one PID would measure only
the pm2-style supervisor and silently undercount. This script wraps the whole
process tree in a `systemd-run --scope` cgroup and reads that cgroup's own
`cpu.stat` — it aggregates every process in the tree automatically, including ones
forked after the measurement window starts.

Needs the `NUM_IDLE_PROCESSES` override committed to `agent-starter-node`'s
`src/main.ts` alongside this script (default stays 16 everywhere else — this run
uses 1, since 16 idle V8 processes each carry a baseline cost large enough to
distort the floor being measured), and cgroup v2 (`/sys/fs/cgroup/cgroup.controllers`
must exist).

```bash
export NODE_AGENT_DIR=/root/agent-starter-node   # the checkout with BENCHMARK_MODE
export LIVEKIT_URL=... LIVEKIT_API_KEY=... LIVEKIT_API_SECRET=...
export CALLER_AUDIO=/root/go-agent-worker/bench/caller.ogg   # SAME file as ablate.sh

pm2 stop survey-agent   # the script refuses to start otherwise
bash bench/node-baseline.sh
```

Reads `idle` and `call` rows against Go's from `bench/ablate.sh`. If Node's idle
cost lands near Go's ~0.13 cores/call, both stacks pay the same WebRTC/media floor
and there's no CPU advantage left for Go to find there. If Node's is near zero,
Go is carrying a fixable per-track cost — see `bench/README.md`'s own
`summary.md` output for the worked comparison.

## What this does not measure

- **Anything comparable to the Node baseline yet.** Node runs
  `inference.TurnDetector()` in-process; this worker still delegates endpointing to
  Sarvam (ROADMAP item 1). Per ADR-019 precondition 1, reach parity before quoting
  any ratio — and the cheap way is running *Node without its turn detector*, not
  building a turn detector in Go, which sherpa does not ship
  ([ADR-018](../docs/DECISIONS.md#adr-018)).
- **Contention.** This is per-call cost at N=1, not behaviour at N=200. Density
  needs many concurrent pipelines in one process (ROADMAP item 4), which needs a
  multi-room mode `cmd/agent` does not have — it joins one room from `LIVEKIT_ROOM`.
- **Dispatch.** Not needed for density, per
  [ADR-001](../docs/DECISIONS.md#adr-001). Don't build it before the gate answers.

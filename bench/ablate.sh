#!/usr/bin/env bash
# Ablation harness for docs/DECISIONS.md ADR-019 precondition 3.
#
# Answers one question: how much of this worker's per-call CPU is native inference
# and codec work (irreducible — identical C libraries in Go and in Node), and how
# much is orchestration (the only part a language change can improve)?
#
# It answers it by DIFFERENCE, not by attribution, because attribution is not
# available here. Go's CPU profiler does not unwind C stacks, so time inside
# sherpa-onnx and libopus/soxr collapses into runtime.cgocall. Both are cgo, so
# pprof is blind to precisely the cost that decides Gate 1 — and blind in Go's
# favour, which is the confound direction ADR-019 exists to prevent. Toggling the
# native stages off and subtracting is the measurement that does not lie.
#
#   run    VAD_ENABLED  GTCRN_ENABLED  isolates
#   idle   false        false          process floor: sockets, runtime, no call
#   A      false        false          orchestration + codec + provider I/O
#   B      true         false          A + TEN VAD inference
#   C      true         true           B + GTCRN speech enhancement
#
#   A - idle  = per-call cost Go could plausibly improve
#   B - A     = TEN VAD inference (irreducible)
#   C - B     = GTCRN enhancement (irreducible)
#
# If (B-A) + (C-B) dominates (C - idle), Go's density ceiling sits near Node's and
# ADR-019 says stop. Splitting codec from orchestration inside A needs the pprof
# profile this script also captures (Go frames are trustworthy; C frames are not) or
# `perf record -g` against the container.
#
# Reported as CPU-seconds per call-minute, never percentages: .19 has 16 cores and
# .20 has 32, and a percentage silently means a different amount of work on each.

set -euo pipefail

RUNNER="${RUNNER:-compose}"          # compose | binary
DURATION="${DURATION:-180}"          # seconds of live call per run
SETTLE="${SETTLE:-20}"               # seconds after healthz before the t0 reading
COOLDOWN="${COOLDOWN:-20}"           # seconds between runs
OPS_URL="${OPS_URL:-http://127.0.0.1:8080}"
PPROF_SECONDS="${PPROF_SECONDS:-60}" # 0 disables profile capture
RUNS="${RUNS:-idle A B C}"
COMPOSE_SERVICE="${COMPOSE_SERVICE:-agent}"
BINARY="${BINARY:-./bin/agent}"
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-2}"

# The caller side. This repo has no audio generator: the worker joins a room and
# waits. Something must publish real speech into LIVEKIT_ROOM for the duration of
# each run, and it must be the SAME audio every run or the comparison is void.
#
# Deliberately has no default. A wrong default here produces a run that looks
# successful and measures an idle worker four times, which is the single most
# expensive mistake this harness could make. METRICS.md is explicit that silence
# under-reports CPU badly.
CALLER_CMD="${CALLER_CMD:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

if [ -z "$CALLER_CMD" ]; then
  cat >&2 <<'EOF'
CALLER_CMD is not set. It must be a command that publishes real speech into the
room the worker joins (LIVEKIT_ROOM), and runs until killed.

Verify the exact flags for your CLI version first — `lk room join --help` — then
export something equivalent to:

  export CALLER_CMD='lk room join --identity bench-caller \
      --publish /path/to/hindi-speech-loop.ogg --room agent-spike-1'

Requirements that make or break the measurement:
  - real speech, not silence or a tone: silence under-reports CPU badly, and a
    tone never triggers endpointing, so no LLM or TTS turn ever runs
  - the SAME file at the SAME length for every run
  - long enough to loop for DURATION without ending early
EOF
  exit 2
fi

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_DIR="$SCRIPT_DIR/results/$RUN_ID"
mkdir -p "$OUT_DIR"
CSV="$OUT_DIR/ablation.csv"
echo "run,vad,gtcrn,call_seconds,cpu_seconds,cpu_s_per_call_min,cores_per_call,rss_start_mb,rss_peak_mb,goroutines_peak,restarts" >"$CSV"

echo "=== Ablation run $RUN_ID ==="
echo "Runner: $RUNNER   duration: ${DURATION}s/run   settle: ${SETTLE}s   runs: $RUNS"
echo "Results: $OUT_DIR"
echo

WORKER_PID=""
OVERRIDE_FILE=""

scrape() {
  # Emits "cpu_seconds rss_bytes goroutines" from the Prometheus process collector
  # already registered in cmd/agent/main.go. Reading the worker's own counter is
  # more reliable than sampling /proc from outside: it survives the container's PID
  # namespace and counts user+system time for the whole process.
  curl -fsS --max-time 5 "$OPS_URL/metrics" 2>/dev/null | awk '
    /^process_cpu_seconds_total /     { cpu = $2 }
    /^process_resident_memory_bytes / { rss = $2 }
    /^go_goroutines /                 { gor = $2 }
    END { if (cpu == "") exit 1; printf "%s %s %s\n", cpu, rss, gor }'
}

restart_count() {
  if [ "$RUNNER" = "compose" ]; then
    docker inspect -f '{{.RestartCount}}' "$(container_id)" 2>/dev/null || echo 0
  else
    echo 0
  fi
}

container_id() {
  docker compose -f "$REPO_DIR/docker-compose.yml" ${OVERRIDE_FILE:+-f "$OVERRIDE_FILE"} \
    ps -q "$COMPOSE_SERVICE" 2>/dev/null
}

start_worker() {
  local vad="$1" gtcrn="$2" tag="$3"

  if [ "$RUNNER" = "compose" ]; then
    # An override file rather than `docker compose run -e`: `run` starts a
    # one-off container that `ps -q` does not report, and restart:unless-stopped
    # from the base file would silently restart a crashed worker mid-run, zeroing
    # process_cpu_seconds_total and making the delta meaningless. Pinning
    # restart:"no" here means a crash shows up as a failure instead.
    OVERRIDE_FILE="$OUT_DIR/compose.override.$tag.yml"
    cat >"$OVERRIDE_FILE" <<EOF
services:
  $COMPOSE_SERVICE:
    restart: "no"
    environment:
      VAD_ENABLED: "$vad"
      GTCRN_ENABLED: "$gtcrn"
      PPROF_ENABLED: "true"
EOF
    docker compose -f "$REPO_DIR/docker-compose.yml" -f "$OVERRIDE_FILE" \
      up -d --force-recreate "$COMPOSE_SERVICE" >>"$OUT_DIR/$tag-worker.log" 2>&1
  else
    [ -x "$BINARY" ] || { echo "binary not found or not executable: $BINARY" >&2; exit 1; }
    ( cd "$REPO_DIR" && VAD_ENABLED="$vad" GTCRN_ENABLED="$gtcrn" PPROF_ENABLED=true \
        "$BINARY" >>"$OUT_DIR/$tag-worker.log" 2>&1 ) &
    WORKER_PID=$!
  fi
}

stop_worker() {
  if [ "$RUNNER" = "compose" ]; then
    docker compose -f "$REPO_DIR/docker-compose.yml" ${OVERRIDE_FILE:+-f "$OVERRIDE_FILE"} \
      down --timeout 15 >/dev/null 2>&1 || true
  elif [ -n "$WORKER_PID" ]; then
    kill -TERM "$WORKER_PID" 2>/dev/null || true
    wait "$WORKER_PID" 2>/dev/null || true
    WORKER_PID=""
  fi
}

wait_healthy() {
  local deadline=$((SECONDS + 90))
  until curl -fsS --max-time 3 "$OPS_URL/healthz" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "  FAIL worker did not become healthy within 90s — see $OUT_DIR/*-worker.log" >&2
      return 1
    fi
    sleep 2
  done
}

CALLER_PID=""
SAMPLER_PID=""
cleanup() {
  [ -n "$CALLER_PID" ] && kill -TERM "$CALLER_PID" 2>/dev/null || true
  [ -n "$SAMPLER_PID" ] && kill -TERM "$SAMPLER_PID" 2>/dev/null || true
  stop_worker
}
trap cleanup EXIT INT TERM

for run in $RUNS; do
  case "$run" in
    idle) vad=false; gtcrn=false; with_call=0 ;;
    A)    vad=false; gtcrn=false; with_call=1 ;;
    B)    vad=true;  gtcrn=false; with_call=1 ;;
    C)    vad=true;  gtcrn=true;  with_call=1 ;;
    *)    echo "unknown run '$run' (expected idle|A|B|C)" >&2; exit 1 ;;
  esac

  echo "--- run $run (VAD=$vad GTCRN=$gtcrn, call=$with_call) ---"
  start_worker "$vad" "$gtcrn" "$run"
  wait_healthy || { stop_worker; exit 1; }

  restarts_before="$(restart_count)"

  # Settle before t0: the first seconds include model load, socket handshakes and
  # JIT-ish warmup that belong to startup, not to a call.
  echo "  settling ${SETTLE}s..."
  sleep "$SETTLE"

  read -r cpu0 rss0 _gor0 <<<"$(scrape)" || { echo "  FAIL could not scrape $OPS_URL/metrics" >&2; stop_worker; exit 1; }

  # Peak RSS and goroutines have to be sampled: /metrics is a point-in-time gauge
  # and the interesting value is the maximum during the call, not at the end.
  PEAK_FILE="$OUT_DIR/$run-samples.csv"
  echo "epoch,cpu_seconds,rss_bytes,goroutines" >"$PEAK_FILE"
  (
    while :; do
      if s="$(scrape)"; then
        echo "$(date +%s),${s// /,}" >>"$PEAK_FILE"
      fi
      sleep "$SAMPLE_INTERVAL"
    done
  ) &
  SAMPLER_PID=$!

  if [ "$with_call" -eq 1 ]; then
    echo "  starting caller for ${DURATION}s..."
    ( eval "$CALLER_CMD" >>"$OUT_DIR/$run-caller.log" 2>&1 ) &
    CALLER_PID=$!
  fi

  if [ "$PPROF_SECONDS" -gt 0 ]; then
    # Captured mid-run so the profile covers steady state. Go frames in this
    # profile are trustworthy; anything under runtime.cgocall is not (see header).
    ( sleep 5
      curl -fsS --max-time $((PPROF_SECONDS + 30)) \
        -o "$OUT_DIR/$run-cpu.pprof" \
        "$OPS_URL/debug/pprof/profile?seconds=$PPROF_SECONDS" >/dev/null 2>&1 || true
      curl -fsS --max-time 15 -o "$OUT_DIR/$run-heap.pprof" \
        "$OPS_URL/debug/pprof/heap" >/dev/null 2>&1 || true ) &
  fi

  sleep "$DURATION"

  if [ -n "$CALLER_PID" ]; then
    kill -TERM "$CALLER_PID" 2>/dev/null || true
    wait "$CALLER_PID" 2>/dev/null || true
    CALLER_PID=""
  fi

  read -r cpu1 rss1 _gor1 <<<"$(scrape)" || { echo "  FAIL could not scrape at t1" >&2; stop_worker; exit 1; }

  kill -TERM "$SAMPLER_PID" 2>/dev/null || true
  wait "$SAMPLER_PID" 2>/dev/null || true
  SAMPLER_PID=""

  restarts_after="$(restart_count)"
  if [ "$restarts_after" != "$restarts_before" ]; then
    echo "  WARN worker restarted during this run ($restarts_before -> $restarts_after)." >&2
    echo "       process_cpu_seconds_total reset, so this row is void. Check $OUT_DIR/$run-worker.log." >&2
  fi

  peak_rss="$(awk -F, 'NR>1 && $3>m {m=$3} END {print m+0}' "$PEAK_FILE")"
  peak_gor="$(awk -F, 'NR>1 && $4>m {m=$4} END {print m+0}' "$PEAK_FILE")"

  awk -v run="$run" -v vad="$vad" -v gtcrn="$gtcrn" -v dur="$DURATION" \
      -v c0="$cpu0" -v c1="$cpu1" -v r0="$rss0" -v rp="$peak_rss" \
      -v gp="$peak_gor" -v rs="$restarts_after" '
    BEGIN {
      cpu = c1 - c0
      permin = (dur > 0) ? cpu / (dur / 60.0) : 0
      printf "%s,%s,%s,%d,%.3f,%.3f,%.4f,%.1f,%.1f,%d,%s\n",
        run, vad, gtcrn, dur, cpu, permin, permin/60.0, r0/1048576, rp/1048576, gp, rs
    }' >>"$CSV"

  tail -1 "$CSV" | awk -F, '{ printf "  cpu %.2fs over %ss = %.2f cpu-s/call-min (%.3f cores/call), rss peak %.0fMB\n", $5, $4, $6, $7, $9 }'

  stop_worker
  echo "  cooling down ${COOLDOWN}s..."
  sleep "$COOLDOWN"
  echo
done

trap - EXIT INT TERM
cleanup

# --- Report -------------------------------------------------------------------
SUMMARY="$OUT_DIR/summary.md"
{
  echo "# Ablation — $RUN_ID"
  echo
  echo "Method: docs/DECISIONS.md ADR-019 precondition 3. CPU by difference, because"
  echo "Go's profiler cannot attribute cgo time (sherpa-onnx, libopus, soxr)."
  echo
  echo "Duration ${DURATION}s/run, settle ${SETTLE}s, runner \`$RUNNER\`."
  echo "Caller: \`$CALLER_CMD\`"
  echo
  column -s, -t <"$CSV" | sed 's/^/    /'
  echo
  awk -F, '
    NR>1 { cpu[$1] = $6 }
    END {
      print "## Derived"
      print ""
      if (!("A" in cpu)) { print "Run A missing — cannot derive."; exit }
      idle = ("idle" in cpu) ? cpu["idle"] : 0
      if (!("idle" in cpu)) print "> No idle run: figures below include the process floor.\n"
      printf "| Component | cpu-s/call-min | share of full pipeline |\n"
      printf "|---|---|---|\n"
      full = (("C" in cpu) ? cpu["C"] : (("B" in cpu) ? cpu["B"] : cpu["A"])) - idle
      a_net = cpu["A"] - idle
      printf "| Orchestration + codec + provider I/O (A - idle) | %.3f | %.1f%% |\n", a_net, (full>0 ? 100*a_net/full : 0)
      if ("B" in cpu) {
        v = cpu["B"] - cpu["A"]
        printf "| TEN VAD inference (B - A) | %.3f | %.1f%% |\n", v, (full>0 ? 100*v/full : 0)
      }
      if ("C" in cpu && "B" in cpu) {
        g = cpu["C"] - cpu["B"]
        printf "| GTCRN enhancement (C - B) | %.3f | %.1f%% |\n", g, (full>0 ? 100*g/full : 0)
      }
      printf "| **Full pipeline (net of idle)** | **%.3f** | 100%% |\n", full
      print ""
      # The failure this harness is most likely to hide: the caller never published
      # audio, so every run measured an idle worker and the table looks plausible.
      # A live call runs STT streaming, an LLM turn and TTS synthesis; it cannot cost
      # almost nothing above the floor.
      if (idle > 0 && a_net < 0.25 * idle) {
        print "> **SUSPECT: run A barely exceeded the idle floor.** A live call should cost"
        print "> substantially more than an idle process. Check that CALLER_CMD actually"
        print "> published audio into LIVEKIT_ROOM and that the worker logged transcripts"
        print "> — see the *-caller.log and *-worker.log files. Treat these rows as void"
        print "> until that is confirmed.\n"
      }
      if (full > 0) {
        native = full - a_net
        printf "Native, irreducible (identical C libraries in Node): **%.1f%%** of per-call CPU.\n", 100*native/full
        printf "Addressable by a language change: **%.1f%%**.\n\n", 100*a_net/full
        print "Read against ADR-019: the addressable share bounds what a Go rewrite can"
        print "win on CPU. It does not bound the memory win, which is separate and larger."
        print ""
        cores = full/60.0
        if (cores > 0) printf "Implied density at this per-call cost: **%.1f calls/core** (1 / %.4f cores per call).\n", 1/cores, cores
      }
    }' "$CSV"
  echo
  echo "## Caveats that apply to every row"
  echo
  echo "- **Not yet comparable to the Node baseline.** Node runs \`inference.TurnDetector()\`"
  echo "  in-process; this worker still delegates endpointing to Sarvam (ROADMAP item 1)."
  echo "  ADR-019 precondition 1: bring them to parity — cheapest by running Node without"
  echo "  its turn detector — before quoting any ratio."
  echo "- **Single call.** This measures per-call cost at N=1, not contention at N=200."
  echo "  Density needs many concurrent pipelines (ROADMAP item 4)."
  echo "- **Idle subtraction assumes VAD/GTCRN cost ~0 with no audio arriving.** They are"
  echo "  driven by submitted frames, so this holds, but one idle run with VAD=true will"
  echo "  confirm it if a number looks wrong."
  echo "- **Provider I/O is in A and is not orchestration.** Sarvam WebSocket traffic and"
  echo "  Gemini calls cost CPU in any language. A pprof read of A splits them out."
} >"$SUMMARY"

echo "=== Complete ==="
cat "$SUMMARY"
echo
echo "Wrote $SUMMARY"
if [ "$PPROF_SECONDS" -gt 0 ]; then
  echo "Profiles: $OUT_DIR/*-cpu.pprof  (go tool pprof -http=:9000 <file>)"
fi

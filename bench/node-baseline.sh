#!/usr/bin/env bash
# Node/LiveKit baseline for docs/DECISIONS.md ADR-019, precondition-adjacent to
# bench/ablate.sh: measures the SAME two configurations against the production
# Node/LiveKit stack (agent-starter-node), so Go's N=1 ablation numbers have
# something honest to sit next to instead of standing alone against Node's N=85
# figure from the Phase 1 saturation benchmark.
#
# Node forks a separate OS process per job - established earlier in this project's
# own investigation: the pm2-managed process is only the master/supervisor.
# Sampling a single PID's /proc/<pid>/stat would measure an empty supervisor and
# silently undercount by most of the real cost. This script instead wraps the
# whole process tree in one systemd-run --scope cgroup and reads that cgroup's own
# cpu.stat, which aggregates every process in the tree - master, idle pool, and
# the job process - automatically, including ones that fork after measurement
# starts. Requires cgroup v2 (the unified hierarchy; Debian 12 defaults to it).
#
# Two configs, matching bench/ablate.sh's naming:
#   idle - Node joins a room via automatic dispatch, speaks its unprompted
#          greeting, then sits with no caller. Isolates whatever WebRTC/media
#          floor Node pays on its own audio-publish path - the same question the
#          Go ablation answered for the Go worker, where it turned out to be
#          ~0.13 cores/call from a per-track silence-encoding loop in
#          server-sdk-go's PCMLocalTrack (see idle-cpu.pprof from that run).
#   call - the same caller.ogg used with bench/ablate.sh, published into a fresh
#          room, driving one real conversation end to end.
#
# No CPU quota is applied to the worker being measured, deliberately: capping it
# would distort the very floor this script exists to measure, and bench/ablate.sh
# does not cap the Go worker either (a plain container, no CPUQuota) - so leaving
# Node uncapped is what keeps the two numbers comparable.
#
# Before running:
#   1. Stop the production survey-agent pm2 process. Two workers registered under
#      the same agent name compete for dispatch, and both would try to bind
#      HEALTH_PORT - this script refuses to start if anything already answers there.
#   2. Have the SAME caller.ogg built for bench/ablate.sh ready (CALLER_AUDIO).
#   3. NUM_IDLE_PROCESSES must be wired into src/main.ts (see the ADR-019 commit
#      that added it) - without it this still runs, just with 16 idle V8 processes
#      contributing their own baseline cost on top of the floor being measured.

set -euo pipefail

: "${NODE_AGENT_DIR:?export NODE_AGENT_DIR - path to the agent-starter-node checkout (the one with src/main.ts and BENCHMARK_MODE support), e.g. /root/agent-starter-node}"
: "${LIVEKIT_URL:?export LIVEKIT_URL first, matching the .env in NODE_AGENT_DIR}"
: "${LIVEKIT_API_KEY:?export LIVEKIT_API_KEY first}"
: "${LIVEKIT_API_SECRET:?export LIVEKIT_API_SECRET first}"

LK="${LK:-lk}"
AGENT_NAME="${AGENT_NAME:-survey-agent}"
DURATION="${DURATION:-180}"
# Longer than bench/ablate.sh's 20s settle on purpose: main.ts calls
# session.generateReply() unconditionally right after connecting, so every Node
# session opens with an unprompted spoken greeting. The measurement window must
# start after that synthesis finishes, or the "idle" row is really "idle plus one
# greeting" and reads high for a reason that has nothing to do with the floor.
SETTLE="${SETTLE:-30}"
COOLDOWN="${COOLDOWN:-20}"
HEALTH_PORT="${HEALTH_PORT:-8081}"
RUNS="${RUNS:-idle call}"
IDLE_ROOM="${IDLE_ROOM:-node-bench-idle}"
CALL_ROOM="${CALL_ROOM:-node-bench-call}"
# Required only for the "call" run. Must be the SAME file bench/ablate.sh used -
# a different recording is a different amount of speech, which is a different
# amount of work, which breaks the comparison the same way an uneven transcript
# count would inside bench/ablate.sh itself.
CALLER_AUDIO="${CALLER_AUDIO:-}"

command -v "$LK" >/dev/null 2>&1 || { echo "lk not found on PATH (set LK=/path/to/lk)"; exit 1; }
command -v systemd-run >/dev/null 2>&1 || { echo "systemd-run not found - this script needs cgroup accounting"; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found - this script needs cgroup accounting"; exit 1; }

if [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
  echo "This host is not on cgroup v2 (the unified hierarchy) - the cpu.stat" >&2
  echo "accounting this script relies on will not work here." >&2
  exit 1
fi

if [ ! -f "$NODE_AGENT_DIR/src/main.ts" ]; then
  echo "NODE_AGENT_DIR ($NODE_AGENT_DIR) does not look like an agent-starter-node" >&2
  echo "checkout - src/main.ts was not found there." >&2
  exit 1
fi

if [[ " $RUNS " == *" call "* ]] && [ -z "$CALLER_AUDIO" ]; then
  echo "CALLER_AUDIO is not set. It must be the SAME .ogg file used with" >&2
  echo "bench/ablate.sh's CALLER_CMD - see that run's results directory." >&2
  exit 1
fi
if [ -n "$CALLER_AUDIO" ] && [ ! -f "$CALLER_AUDIO" ]; then
  echo "CALLER_AUDIO points at '$CALLER_AUDIO', which does not exist." >&2
  exit 1
fi

if curl -fsS --max-time 2 "http://127.0.0.1:${HEALTH_PORT}/" >/dev/null 2>&1; then
  echo "Something is already answering on :${HEALTH_PORT} - most likely the" >&2
  echo "production survey-agent pm2 process. Stop it first:" >&2
  echo "    pm2 stop survey-agent" >&2
  echo "Two workers under the same agent name would both compete for dispatch," >&2
  echo "and this script's own instance would fail to bind the port." >&2
  exit 1
fi

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$SCRIPT_DIR/results/node-$RUN_ID"
mkdir -p "$OUT_DIR"
CSV="$OUT_DIR/node-baseline.csv"
echo "run,room,call_seconds,cpu_seconds,cpu_s_per_call_min,cores_per_call,llm_turns" >"$CSV"

echo "=== Node baseline run $RUN_ID ==="
echo "Node agent dir: $NODE_AGENT_DIR"
echo "Results: $OUT_DIR"
echo

# Reads the cgroup v2 cpu.stat usage_usec field for a systemd scope unit -
# cumulative microseconds for every process that has ever been part of the
# cgroup, including the master, the idle pool, and any job process forked after
# this script started reading. Persists across a child process exiting, which is
# what makes this tree-safe without polling /proc for new PIDs.
cgroup_usage_usec() {
  local unit="$1" cg_path stat_file
  cg_path="$(systemctl show "${unit}.scope" -p ControlGroup --value 2>/dev/null || true)"
  [ -z "$cg_path" ] && { echo ""; return; }
  stat_file="/sys/fs/cgroup${cg_path}/cpu.stat"
  [ -f "$stat_file" ] || { echo ""; return; }
  awk '/^usage_usec/ {print $2}' "$stat_file"
}

UNIT=""
CALLER_PID=""
cleanup() {
  [ -n "$CALLER_PID" ] && kill -TERM "$CALLER_PID" 2>/dev/null || true
  [ -n "$UNIT" ] && systemctl stop "${UNIT}.scope" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

for run in $RUNS; do
  case "$run" in
    idle) room="$IDLE_ROOM"; with_call=0 ;;
    call) room="$CALL_ROOM"; with_call=1 ;;
    *) echo "unknown run '$run' (expected idle|call)" >&2; exit 1 ;;
  esac

  echo "--- run $run (room=$room, call=$with_call) ---"
  UNIT="bench-node-${RUN_ID}-${run}"
  WORKER_LOG="$OUT_DIR/$run-worker.log"
  CALLER_LOG="$OUT_DIR/$run-caller.log"

  # NUM_IDLE_PROCESSES=1 needs the small override added to src/main.ts alongside
  # this script - the default there stays 16 for every other invocation. Run via
  # `node src/main.ts start` directly rather than pm2, matching the exact
  # invocation bench/README.md already documents for a benchmark instance, just
  # wrapped in a scope instead of pm2's supervision so its cgroup is ours to read.
  systemd-run --scope --quiet --unit="$UNIT" --working-directory="$NODE_AGENT_DIR" \
    -- env BENCHMARK_MODE=1 NUM_IDLE_PROCESSES=1 node src/main.ts start \
    >"$WORKER_LOG" 2>&1 &

  deadline=$((SECONDS + 60))
  until curl -fsS --max-time 2 "http://127.0.0.1:${HEALTH_PORT}/" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "  FAIL worker did not become healthy within 60s - see $WORKER_LOG" >&2
      cleanup; UNIT=""; exit 1
    fi
    sleep 2
  done
  echo "  worker healthy"

  # Creating the room triggers automatic dispatch to $AGENT_NAME. `lk room join`
  # has no --agent-name flag (unlike `lk perf agent-load-test`); this relies on
  # the project's default automatic-dispatch behaviour, same as every prior
  # benchmark run in this project against this same worker.
  if [ "$with_call" -eq 1 ]; then
    ( "$LK" room join --identity bench-caller --publish "$CALLER_AUDIO" "$room" \
        >"$CALLER_LOG" 2>&1 ) &
  else
    ( timeout $((SETTLE + DURATION + 10)) \
        "$LK" room join --identity bench-idle-observer "$room" >"$CALLER_LOG" 2>&1 ) &
  fi
  CALLER_PID=$!

  # Fail fast if dispatch never happens, rather than measuring an undispatched
  # worker for the full settle+duration window - the exact failure bench/ablate.sh
  # hit once already, just one layer up the stack here (dispatch, not publish).
  deadline=$((SECONDS + 20))
  until grep -q "survey-agent starting" "$WORKER_LOG" 2>/dev/null; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "  FAIL no job dispatched to $AGENT_NAME within 20s of room '$room'" >&2
      echo "       being created. Check $WORKER_LOG, and confirm this LiveKit" >&2
      echo "       project has no explicit dispatch rule restricting automatic" >&2
      echo "       dispatch." >&2
      exit 1
    fi
    sleep 1
  done
  echo "  dispatched"

  echo "  settling ${SETTLE}s (absorbs the unprompted greeting)..."
  sleep "$SETTLE"

  t0="$(cgroup_usage_usec "$UNIT")"
  if [ -z "$t0" ]; then
    echo "  FAIL could not read cgroup cpu.stat for $UNIT" >&2
    exit 1
  fi

  echo "  measuring ${DURATION}s..."
  sleep "$DURATION"

  t1="$(cgroup_usage_usec "$UNIT")"

  kill -TERM "$CALLER_PID" 2>/dev/null || true
  wait "$CALLER_PID" 2>/dev/null || true
  CALLER_PID=""

  # LLM-turn count is this script's equivalent of bench/ablate.sh's transcript
  # count: evidence real work happened, and a way to sanity-check the two runs did
  # comparable amounts of it. "ttftMs" appears once per LLM-metrics line that
  # metrics.logMetrics() emits (see main.ts's MetricsCollected handler).
  llm_turns="$(grep -cE '"ttftMs"' "$WORKER_LOG" 2>/dev/null || true)"
  llm_turns="${llm_turns:-0}"

  if [ "$with_call" -eq 1 ] && [ "$llm_turns" -eq 0 ]; then
    echo "  WARN no LLM turns observed in $WORKER_LOG. Check $CALLER_LOG - a" >&2
    echo "       missing/misnamed CALLER_AUDIO file shows up there as" >&2
    echo "       'stat ...: no such file or directory' after 'connected to room'." >&2
    echo "       Treat this row as void until that is confirmed." >&2
  fi
  [ "$with_call" -eq 1 ] && echo "  ($llm_turns LLM turn(s) this run)"

  awk -v run="$run" -v room="$room" -v dur="$DURATION" -v u0="$t0" -v u1="$t1" \
      -v turns="$llm_turns" '
    BEGIN {
      cpu = (u1 - u0) / 1000000.0
      permin = (dur > 0) ? cpu / (dur / 60.0) : 0
      printf "%s,%s,%d,%.3f,%.3f,%.4f,%d\n", run, room, dur, cpu, permin, permin/60.0, turns
    }' >>"$CSV"

  tail -1 "$CSV" | awk -F, '{ printf "  cpu %.2fs over %ss = %.2f cpu-s/call-min (%.3f cores/call)\n", $4, $3, $5, $6 }'

  systemctl stop "${UNIT}.scope" >/dev/null 2>&1 || true
  UNIT=""
  echo "  cooling down ${COOLDOWN}s..."
  sleep "$COOLDOWN"
  echo
done

trap - EXIT INT TERM

SUMMARY="$OUT_DIR/summary.md"
{
  echo "# Node baseline - $RUN_ID"
  echo
  echo "Tree-safe CPU accounting via cgroup v2 (systemd-run --scope + cpu.stat"
  echo "usage_usec). Node forks a separate OS process per job; a single-PID sample"
  echo "would measure only the supervisor and silently undercount."
  echo
  echo "Node agent dir: \`$NODE_AGENT_DIR\`"
  [ -n "$CALLER_AUDIO" ] && echo "Caller audio: \`$CALLER_AUDIO\` (must match bench/ablate.sh's file)"
  echo
  column -s, -t <"$CSV" | sed 's/^/    /'
  echo
  awk -F, 'NR>1 {c[$1]=$6}
    END {
      if (("idle" in c) && ("call" in c)) {
        printf "\n## Compared to Go (bench/ablate.sh, config B: VAD on, GTCRN off)\n\n"
        printf "| | cores/call | calls/core |\n|---|---|---|\n"
        printf "| Node idle (this run) | %.4f | %.1f |\n", c["idle"], (c["idle"]>0 ? 1/c["idle"] : 0)
        printf "| Node call (this run) | %.4f | %.1f |\n", c["call"], (c["call"]>0 ? 1/c["call"] : 0)
        printf "| Go idle (bench/ablate.sh) | 0.1293 | 7.7 |\n"
        printf "| Go call, config B (bench/ablate.sh) | 0.2286 | 4.4 |\n"
      }
    }' "$CSV"
  echo
  echo "## Reading this against ADR-019"
  echo
  echo "If Node's idle cost lands close to Go's (~0.13 cores/call), both stacks pay"
  echo "the same WebRTC/media floor. It is inherent to keeping an audio track alive,"
  echo "identical native code either way, and there is no CPU advantage left for Go"
  echo "to find there - only the memory difference remains."
  echo
  echo "If Node's idle cost is near zero, Go is carrying a fixable per-track cost"
  echo "Node does not pay. Closing that gap would move Go from ~4.4 to roughly"
  echo "~10 calls/core - still short of ADR-019's 14-calls/core bar to proceed, but"
  echo "no longer a clear stop either."
  echo
  echo "This is still an N=1-vs-N=1 comparison on both sides. Node's 7.07 calls/core"
  echo "figure came from 85 concurrent calls at saturation, where fixed per-process"
  echo "cost amortizes across a loaded box - it is not directly comparable to either"
  echo "number here. The real Gate 1 answer needs both stacks measured at the same"
  echo "concurrency (ROADMAP item 4), not N=1."
  echo
  echo "## Caveats"
  echo
  echo "- **Idle includes one unprompted greeting.** main.ts calls generateReply()"
  echo "  unconditionally on connect. SETTLE=$SETTLE was chosen to run past it, but"
  echo "  if the idle row still looks high, check $OUT_DIR/idle-worker.log for a"
  echo "  ttftMs/ttfbMs pair landing after settle rather than during it."
  echo "- **llm_turns is the parity check.** If it differs noticeably between this"
  echo "  run's call and bench/ablate.sh's 37 transcripts, the two files' segmentation"
  echo "  diverged and the comparison is weaker, not necessarily wrong."
  echo "- **No CPU quota was applied.** Deliberately - capping the worker being"
  echo "  measured would distort the floor, and bench/ablate.sh does not cap the Go"
  echo "  worker either."
} >"$SUMMARY"

echo "=== Complete ==="
cat "$SUMMARY"

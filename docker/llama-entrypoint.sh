#!/bin/sh
set -eu

SERVER_BIN="${LLAMA_SERVER_BIN:-}"
if [ -z "$SERVER_BIN" ]; then
  if command -v llama-server >/dev/null 2>&1; then
    SERVER_BIN="$(command -v llama-server)"
  elif command -v server >/dev/null 2>&1; then
    SERVER_BIN="$(command -v server)"
  else
    SERVER_BIN="/app/llama-server"
  fi
fi

CURRENT_MODEL_FILE="${CURRENT_MODEL_FILE:-/models/current-model.txt}"
MODEL_DIR="${MODEL_DIR:-/models}"
HOST="${LLAMA_HOST:-0.0.0.0}"
PORT="${LLAMA_PORT:-8080}"
CTX_SIZE="${LLAMA_CTX_SIZE:-4096}"
THREADS="${LLAMA_THREADS:-4}"
PARALLEL="${LLAMA_PARALLEL:-1}"
N_GPU_LAYERS="${LLAMA_N_GPU_LAYERS:-0}"
MAIN_GPU="${LLAMA_MAIN_GPU:-0}"
FLASH_ATTN="${LLAMA_FLASH_ATTN:-false}"
EXTRA_ARGS="${LLAMA_EXTRA_ARGS:-}"
POLL_SECONDS="${MODEL_POLL_SECONDS:-5}"
AUTO_RELOAD_MODEL="${LLAMA_AUTO_RELOAD_MODEL:-true}"

# KV-cache prefix reuse across requests (--cache-reuse N). Grounded answers
# and their follow-ups share a long system+sources prefix, so reusing it skips
# most prompt processing. Empty or 0 disables the flag.
CACHE_REUSE="${LLAMA_CACHE_REUSE:-}"

# Speculative decoding (needs a llama.cpp build with --spec-type, >= b9274 for
# a leak-free MTP). Examples:
#   LLAMA_SPEC_TYPE=ngram-mod   draftless n-gram lookup; works with ANY model,
#                               shines on grounded/RAG answers that quote the
#                               sources present in the prompt.
#   LLAMA_SPEC_TYPE=draft-mtp   the model's own MTP heads; MTP-native models
#                               only (Qwen3.5/3.6, DeepSeek V3/R1, Gemma 4...).
# LLAMA_SPEC_DRAFT_MODEL names a .gguf in the models dir for the draft-* types
# that need a separate draft model. If llama-server dies right after start
# twice in a row, the spec flags are dropped automatically so a model without
# MTP heads (or an old build) still serves.
SPEC_TYPE="${LLAMA_SPEC_TYPE:-}"
SPEC_DRAFT_MODEL="${LLAMA_SPEC_DRAFT_MODEL:-}"
SPEC_DRAFT_N_MAX="${LLAMA_SPEC_DRAFT_N_MAX:-}"
spec_disabled=0
spec_fail_count=0
server_started_at=0

normalize_ngl() {
  value="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | xargs)"
  # Legacy: many docs used 999 to mean "offload as much as possible".
  # llama.cpp now supports explicit 'auto'/'all'. Prefer 'auto' so the server
  # can fit to the available VRAM.
  if [ "$value" = "999" ]; then
    value="auto"
  fi
  printf '%s' "$value"
}

is_true() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    true|1|yes|on)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

select_model() {
  if [ -f "$CURRENT_MODEL_FILE" ] && [ -s "$CURRENT_MODEL_FILE" ]; then
    model_name="$(tr -d '\r' < "$CURRENT_MODEL_FILE" | head -n 1 | xargs)"
    if [ -n "$model_name" ] && [ -f "$MODEL_DIR/$model_name" ]; then
      printf '%s' "$MODEL_DIR/$model_name"
      return 0
    fi
  fi

  first_model="$(find "$MODEL_DIR" -maxdepth 1 -type f -name '*.gguf' | sort | head -n 1 || true)"
  if [ -n "$first_model" ]; then
    basename "$first_model" > "$CURRENT_MODEL_FILE"
    printf '%s' "$first_model"
    return 0
  fi

  return 1
}

active_model=""
child_pid=""

start_server() {
  model_path="$1"
  echo "Starting llama.cpp with model: $model_path"

  set -- \
    --host "$HOST" \
    --port "$PORT" \
    --model "$model_path" \
    --ctx-size "$CTX_SIZE" \
    --threads "$THREADS" \
    --parallel "$PARALLEL"

  ngl="$(normalize_ngl "$N_GPU_LAYERS")"
  if [ -n "$ngl" ] && [ "$ngl" != "0" ] && [ "$ngl" != "off" ] && [ "$ngl" != "false" ] && [ "$ngl" != "no" ]; then
    set -- "$@" --n-gpu-layers "$ngl" --main-gpu "$MAIN_GPU"
  fi

  case "$(printf '%s' "$FLASH_ATTN" | tr '[:upper:]' '[:lower:]')" in
    true|1|yes|on)
      set -- "$@" --flash-attn on
      ;;
  esac

  if [ -n "$CACHE_REUSE" ] && [ "$CACHE_REUSE" != "0" ]; then
    set -- "$@" --cache-reuse "$CACHE_REUSE"
  fi

  if [ -n "$SPEC_TYPE" ] && [ "$spec_disabled" -eq 0 ]; then
    set -- "$@" --spec-type "$SPEC_TYPE"
    if [ -n "$SPEC_DRAFT_MODEL" ]; then
      set -- "$@" --spec-draft-model "$MODEL_DIR/$SPEC_DRAFT_MODEL"
    fi
    if [ -n "$SPEC_DRAFT_N_MAX" ]; then
      set -- "$@" --spec-draft-n-max "$SPEC_DRAFT_N_MAX"
    fi
    echo "Speculative decoding enabled: --spec-type $SPEC_TYPE"
  fi

  if [ -n "$EXTRA_ARGS" ]; then
    # shellcheck disable=SC2086
    set -- "$@" $EXTRA_ARGS
  fi

  "$SERVER_BIN" "$@" &
  child_pid=$!
  server_started_at="$(date +%s)"
  active_model="$model_path"
}

stop_server() {
  if [ -n "$child_pid" ] && kill -0 "$child_pid" 2>/dev/null; then
    kill "$child_pid"
    wait "$child_pid" || true
  fi
  child_pid=""
}

trap 'stop_server; exit 0' INT TERM

while true; do
  if model_path="$(select_model)"; then
    desired_model="$model_path"
    if [ -n "$active_model" ] && ! is_true "$AUTO_RELOAD_MODEL"; then
      desired_model="$active_model"
    fi

    if [ "$desired_model" != "$active_model" ]; then
      stop_server
      # A different model may well support speculative decoding: try again.
      spec_fail_count=0
      spec_disabled=0
      start_server "$desired_model"
    elif [ -n "$child_pid" ] && ! kill -0 "$child_pid" 2>/dev/null; then
      # Crashed. If speculative decoding is on and the server keeps dying
      # right after start, assume the model/build doesn't support it and
      # retry without the spec flags instead of crash-looping forever.
      uptime="$(( $(date +%s) - server_started_at ))"
      if [ -n "$SPEC_TYPE" ] && [ "$spec_disabled" -eq 0 ] && [ "$uptime" -lt 30 ]; then
        spec_fail_count=$((spec_fail_count + 1))
        if [ "$spec_fail_count" -ge 2 ]; then
          echo "llama-server died ${uptime}s after start twice with --spec-type $SPEC_TYPE;"
          echo "disabling speculative decoding for this model (missing MTP heads or old build?)."
          spec_disabled=1
        fi
      else
        spec_fail_count=0
      fi
      start_server "$desired_model"
    fi
  else
    echo "No GGUF model found in $MODEL_DIR. Waiting..."
    stop_server
  fi
  sleep "$POLL_SECONDS"
done

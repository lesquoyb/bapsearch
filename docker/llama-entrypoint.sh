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

  if [ "$N_GPU_LAYERS" != "0" ]; then
    set -- "$@" --n-gpu-layers "$N_GPU_LAYERS" --main-gpu "$MAIN_GPU"
  fi

  case "$(printf '%s' "$FLASH_ATTN" | tr '[:upper:]' '[:lower:]')" in
    true|1|yes|on)
      set -- "$@" --flash-attn on
      ;;
  esac

  if [ -n "$EXTRA_ARGS" ]; then
    # shellcheck disable=SC2086
    set -- "$@" $EXTRA_ARGS
  fi

  "$SERVER_BIN" "$@" &
  child_pid=$!
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
      start_server "$desired_model"
    elif [ -n "$child_pid" ] && ! kill -0 "$child_pid" 2>/dev/null; then
      start_server "$desired_model"
    fi
  else
    echo "No GGUF model found in $MODEL_DIR. Waiting..."
    stop_server
  fi
  sleep "$POLL_SECONDS"
done

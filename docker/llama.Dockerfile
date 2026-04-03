ARG LLAMA_IMAGE=ghcr.io/ggml-org/llama.cpp:server
FROM ${LLAMA_IMAGE}
COPY llama-entrypoint.sh /config/llama-entrypoint.sh

#!/usr/bin/env bash
# Q4, the local check: is the fake provider's tail realistic, and does
# hedging help against a real backend? Runs streamed requests through the
# gateway to a local Ollama with hedging off and on, at a low rate a CPU
# can serve. Ollama must be running with the model pulled. Raw results go
# to eval/results/q4-hedge-ollama/; `make results` renders them. The run
# itself is eval/hedge/live.sh, shared with eval/hedge/claude.sh.
#
#   OLLAMA_URL=http://127.0.0.1:11434 eval/hedge/ollama.sh
#   RATE=1 DURATION=20s WARMUP=10s eval/hedge/ollama.sh   # a quick smoke
set -euo pipefail

PROVIDER=ollama exec "$(dirname "$0")/live.sh"

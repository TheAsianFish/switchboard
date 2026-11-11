#!/usr/bin/env bash
# Q4 on a real provider: the same hedging off/on comparison as
# eval/hedge/ollama.sh, through the gateway's anthropic provider to Claude
# (MODEL, default claude-haiku-5-5). This calls the paid Anthropic API;
# RESULTS.md's "To measure" gives the estimated cost. Same knobs and
# defaults as ollama.sh (RATE, DURATION, WARMUP, MAX_TOKENS, MODEL, OUT).
# Raw results go to eval/results/q4-hedge-claude/; `make results` renders
# them. The run itself is eval/hedge/live.sh.
#
#   ANTHROPIC_API_KEY=... eval/hedge/claude.sh
set -euo pipefail

if [[ -z ${ANTHROPIC_API_KEY:-} ]]; then
	echo "q4-claude: ANTHROPIC_API_KEY is not set; this run calls the paid Anthropic API." >&2
	exit 1
fi

PROVIDER=anthropic exec "$(dirname "$0")/live.sh"

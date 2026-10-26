#!/usr/bin/env bash
# Captures real provider responses as test fixtures. Each fixture is the raw
# response body as the upstream sent it, plus a .status file with the HTTP
# status code. Nothing here is edited by hand after capture.
#
#   scripts/record-fixtures.sh ollama      # needs Ollama with $OLLAMA_MODEL pulled
#   scripts/record-fixtures.sh anthropic   # needs ANTHROPIC_API_KEY; costs < $0.01
set -euo pipefail

target=${1:?usage: record-fixtures.sh ollama|anthropic}
out=internal/provider/testdata
mkdir -p "$out/openai" "$out/anthropic"

capture() { # capture <file> <url> <json body> [curl args...]
	local file=$1 url=$2 body=$3
	shift 3
	curl -sS -N -o "$file" -w '%{http_code}\n' "$@" \
		-H 'content-type: application/json' --data "$body" "$url" >"$file.status"
	echo "$file ($(cat "$file.status"))"
}

case $target in
ollama)
	base=${OLLAMA_URL:-http://localhost:11434}
	model=${OLLAMA_MODEL:-qwen2.5:0.5b}
	prompt='Name three primary colours, comma separated.'
	o=$out/openai/ollama
	capture "$o-chat.json" "$base/v1/chat/completions" \
		"{\"model\":\"$model\",\"temperature\":0,\"messages\":[{\"role\":\"system\",\"content\":\"Be brief.\"},{\"role\":\"user\",\"content\":\"$prompt\"}]}"
	capture "$o-chat-stream.sse" "$base/v1/chat/completions" \
		"{\"model\":\"$model\",\"temperature\":0,\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"$prompt\"}]}"
	capture "$o-chat-stream-length.sse" "$base/v1/chat/completions" \
		"{\"model\":\"$model\",\"temperature\":0,\"max_tokens\":5,\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"Count from one to twenty in words.\"}]}"
	capture "$o-chat-unknown-model.json" "$base/v1/chat/completions" \
		'{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}'
	a=$out/anthropic/ollama
	hdr=(-H 'x-api-key: ollama' -H 'anthropic-version: 2023-06-01')
	capture "$a-messages.json" "$base/v1/messages" \
		"{\"model\":\"$model\",\"max_tokens\":64,\"temperature\":0,\"system\":\"Be brief.\",\"messages\":[{\"role\":\"user\",\"content\":\"$prompt\"}]}" "${hdr[@]}"
	capture "$a-messages-stream.sse" "$base/v1/messages" \
		"{\"model\":\"$model\",\"max_tokens\":64,\"temperature\":0,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"$prompt\"}]}" "${hdr[@]}"
	capture "$a-messages-stream-max-tokens.sse" "$base/v1/messages" \
		"{\"model\":\"$model\",\"max_tokens\":5,\"temperature\":0,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Count from one to twenty in words.\"}]}" "${hdr[@]}"
	capture "$a-messages-unknown-model.json" "$base/v1/messages" \
		'{"model":"no-such-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' "${hdr[@]}"
	;;
anthropic)
	: "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY is not set}"
	model=${ANTHROPIC_MODEL:-claude-haiku-5-5}
	a=$out/anthropic/claude
	hdr=(-H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01')
	capture "$a-messages.json" https://api.anthropic.com/v1/messages \
		"{\"model\":\"$model\",\"max_tokens\":64,\"system\":\"Be brief.\",\"messages\":[{\"role\":\"user\",\"content\":\"Name three primary colours, comma separated.\"}]}" "${hdr[@]}"
	capture "$a-messages-stream.sse" https://api.anthropic.com/v1/messages \
		"{\"model\":\"$model\",\"max_tokens\":64,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Name three primary colours, comma separated.\"}]}" "${hdr[@]}"
	capture "$a-messages-stream-max-tokens.sse" https://api.anthropic.com/v1/messages \
		"{\"model\":\"$model\",\"max_tokens\":5,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"Count from one to twenty in words.\"}]}" "${hdr[@]}"
	;;
*)
	echo "unknown target $target" >&2
	exit 2
	;;
esac

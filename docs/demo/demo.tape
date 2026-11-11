# VHS script for docs/demo.gif; see docs/demo.md for the steps around it.
Output docs/demo.gif

Set Shell "bash"
Set FontSize 15
Set Width 1100
Set Height 620
Set TypingSpeed 25ms
Set Theme "Dracula"

Hide
Type "source .venv/bin/activate && clear" Enter
Show

Type "# The unmodified OpenAI SDK, streaming from a local model through the gateway" Enter
Type "python3 docs/demo/openai_stream.py" Enter
Sleep 5s

Type "# The unmodified Anthropic SDK, same gateway, same model" Enter
Type "python3 docs/demo/anthropic_stream.py" Enter
Sleep 5s

Type "# A deterministic request twice: the second is a cache hit" Enter
Type `req='{"model":"qwen2.5:0.5b","temperature":0,"max_tokens":32,"messages":[{"role":"user","content":"What is 2+2?"}]}'` Enter
Type `for i in 1 2; do curl -s -o /dev/null -D - localhost:8080/v1/chat/completions -H 'Authorization: Bearer sk-switchboard-dev' -H 'Content-Type: application/json' -d "$req" | grep -i cache-status; done` Enter
Sleep 5s

Type "# A model this key may not use: refused in the client's own error format" Enter
Type `curl -s localhost:8080/v1/messages -H 'x-api-key: sk-switchboard-dev' -H 'Content-Type: application/json' -d '{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'; echo` Enter
Sleep 4s

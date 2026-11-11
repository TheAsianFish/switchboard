"""Stream one reply through the gateway with the unmodified Anthropic SDK."""

from anthropic import Anthropic

client = Anthropic(base_url="http://localhost:8080", api_key="sk-switchboard-dev")
with client.messages.stream(
    model="qwen2.5:0.5b",
    max_tokens=128,
    messages=[{"role": "user", "content": "Name three primary colours."}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
print()

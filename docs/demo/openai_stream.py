"""Stream one reply through the gateway with the unmodified OpenAI SDK."""

from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-switchboard-dev")
stream = client.chat.completions.create(
    model="qwen2.5:0.5b",
    stream=True,
    messages=[{"role": "user", "content": "Name three primary colours."}],
)
for chunk in stream:
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="", flush=True)
print()

# Recording the demo GIF

The README's demo GIF is not captured yet. It is recorded from a script,
so it can be re-recorded after any change and shows only what the commands
really print. [VHS](https://github.com/charmbracelet/vhs) (free, MIT) turns
`docs/demo/demo.tape` into `docs/demo.gif`.

What it shows, in about 40 seconds:

1. the OpenAI Python SDK streaming a reply from `qwen2.5:0.5b` through the
   gateway;
2. the Anthropic Python SDK streaming from the same local model;
3. the same deterministic request twice, the second answered from the
   cache (`X-Switchboard-Cache-Status: hit-exact`);
4. a request for a model the key may not use, refused in the client's own
   error format.

Steps, from the repository root:

```sh
# 1. The stack and a model (as in the README quickstart).
docker compose -f deploy/compose.yaml --profile dev up -d --build
docker compose -f deploy/compose.yaml exec ollama ollama pull qwen2.5:0.5b
python3 -m venv .venv && . .venv/bin/activate && pip install openai anthropic

# 2. Warm the model once, so the recording does not show its load time.
python3 docs/demo/openai_stream.py >/dev/null

# 3. Record (VHS needs ttyd and ffmpeg; `go install
#    github.com/charmbracelet/vhs@latest`, or its Docker image).
vhs docs/demo/demo.tape          # writes docs/demo.gif

# 4. Check it, then reference it in the README's Demo section:
#    ![Both SDKs streaming through the gateway](docs/demo.gif)
```

Keep the GIF under about 2 MB (the tape's width, height and `PlaybackSpeed`
are the knobs). The admin page screenshot (`docs/admin.png`) is taken by
hand from http://127.0.0.1:9090 while `make chaos` runs.

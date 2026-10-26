"""Stream a reply through the gateway with the unmodified OpenAI and
Anthropic Python SDKs and record what each client saw.

Each (SDK, model) pair writes one JSON file. A case passes when the SDK's
own stream parser delivered the text in more than one chunk, reported a
stop reason and usage, and the non-streaming call returned the same kind of
reply. Nothing here patches or configures the SDKs beyond base_url and
api_key.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import platform
import sys
import time
from datetime import datetime, timezone

import anthropic
import openai

PROMPT = "Name three primary colours, comma separated."
MAX_TOKENS = 64


def openai_case(base: str, key: str, model: str) -> dict:
    client = openai.OpenAI(base_url=f"{base}/v1", api_key=key, max_retries=0)
    start = time.perf_counter()
    first = None
    pieces: list[str] = []
    finish = None
    usage = None
    stream = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        max_tokens=MAX_TOKENS,
        temperature=0,
        stream=True,
        stream_options={"include_usage": True},
    )
    for chunk in stream:
        if chunk.usage is not None:
            usage = {"prompt_tokens": chunk.usage.prompt_tokens, "completion_tokens": chunk.usage.completion_tokens}
        for choice in chunk.choices:
            if choice.delta.content:
                first = first or time.perf_counter()
                pieces.append(choice.delta.content)
            if choice.finish_reason:
                finish = choice.finish_reason
    end = time.perf_counter()
    full = client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        max_tokens=MAX_TOKENS,
        temperature=0,
    )
    return {
        "stream": {
            "text": "".join(pieces),
            "chunks": len(pieces),
            "stop_reason": finish,
            "usage": usage,
            "ttft_ms": round((first - start) * 1000, 1) if first else None,
            "total_ms": round((end - start) * 1000, 1),
        },
        "complete": {
            "text": full.choices[0].message.content,
            "stop_reason": full.choices[0].finish_reason,
            "usage": {"prompt_tokens": full.usage.prompt_tokens, "completion_tokens": full.usage.completion_tokens},
            "response_model": full.model,
        },
    }


def anthropic_case(base: str, key: str, model: str) -> dict:
    client = anthropic.Anthropic(base_url=base, api_key=key, max_retries=0)
    start = time.perf_counter()
    first = None
    pieces: list[str] = []
    with client.messages.stream(
        model=model,
        max_tokens=MAX_TOKENS,
        temperature=0,
        messages=[{"role": "user", "content": PROMPT}],
    ) as stream:
        for text in stream.text_stream:
            if text:
                first = first or time.perf_counter()
                pieces.append(text)
        final = stream.get_final_message()
    end = time.perf_counter()
    full = client.messages.create(
        model=model,
        max_tokens=MAX_TOKENS,
        temperature=0,
        messages=[{"role": "user", "content": PROMPT}],
    )
    return {
        "stream": {
            "text": "".join(pieces),
            "chunks": len(pieces),
            "stop_reason": final.stop_reason,
            "usage": {"input_tokens": final.usage.input_tokens, "output_tokens": final.usage.output_tokens},
            "ttft_ms": round((first - start) * 1000, 1) if first else None,
            "total_ms": round((end - start) * 1000, 1),
        },
        "complete": {
            "text": "".join(b.text for b in full.content if b.type == "text"),
            "stop_reason": full.stop_reason,
            "usage": {"input_tokens": full.usage.input_tokens, "output_tokens": full.usage.output_tokens},
            "response_model": full.model,
        },
    }


def passed(result: dict) -> bool:
    s, c = result["stream"], result["complete"]
    out_tokens = (s["usage"] or {}).get("completion_tokens", (s["usage"] or {}).get("output_tokens", 0))
    return bool(s["text"]) and s["chunks"] > 1 and s["stop_reason"] is not None and out_tokens > 0 and bool(c["text"])


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--model", action="append", required=True, help="model=backend description")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    out = pathlib.Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    failures = 0
    for spec in args.model:
        model, _, backend = spec.partition("=")
        for sdk, fn, version in (
            ("openai", openai_case, openai.__version__),
            ("anthropic", anthropic_case, anthropic.__version__),
        ):
            record = {
                "sdk": f"{sdk}-python",
                "sdk_version": version,
                "model": model,
                "backend": backend,
                "prompt": PROMPT,
                "max_tokens": MAX_TOKENS,
                "python": platform.python_version(),
                "recorded_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            }
            try:
                record.update(fn(args.base, args.key, model))
                record["passed"] = passed(record)
            except Exception as exc:  # recorded, not raised: the file says what broke
                record["passed"] = False
                record["error"] = f"{type(exc).__name__}: {exc}"
            failures += not record["passed"]
            name = f"{sdk}--{model.replace(':', '_').replace('/', '_')}.json"
            (out / name).write_text(json.dumps(record, indent=2, sort_keys=True) + "\n")
            status = "ok  " if record["passed"] else "FAIL"
            detail = record.get("error") or repr(record["stream"]["text"][:60])
            print(f"{status} {sdk:9} {model:28} {detail}", file=sys.stderr)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())

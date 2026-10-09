"""Official OpenAI SDK against the actual TLS gateway and a synthetic runtime.

Invoked by integration_test.go. No OpenAI service is contacted. The client and
transport versions are test dependencies, independent of the native SDK.
"""
import asyncio
import json
import logging
import os
from pathlib import Path
import ssl
import subprocess
import sys
import time

os.environ.pop("OPENAI_LOG", None)
logging.disable(logging.CRITICAL)

import httpx
from openai import APIStatusError, AsyncOpenAI, OpenAI

url, ca_file, token_a, token_b = sys.argv[1:]
token = Path(token_a).read_text().strip()
tls = ssl.create_default_context(cafile=ca_file)
messages = [{"role": "user", "content": "SYNTHETIC_PRIVATE_INPUT"}]
tools = [{"type": "function", "function": {
    "name": "lookup_demo", "strict": True,
    "parameters": {"type": "object", "properties": {"code": {"type": "string"}},
                   "required": ["code"], "additionalProperties": False},
}}]


def client_for(key):
    return OpenAI(base_url=url + "/v1", api_key=key, max_retries=0,
                  timeout=10, http_client=httpx.Client(verify=tls, trust_env=False,
                                                       follow_redirects=False, timeout=10))


with httpx.Client(verify=tls, trust_env=False, follow_redirects=False,
                  timeout=10, headers={"Authorization": "Bearer " + token}) as wire:
    capability = wire.get(url + "/local/v1/capabilities").json()
    assert capability["contract_version"] == "1"
    assert capability["object"] == "apostille_local.capabilities"
    assert capability["tool_execution"] == "client"
    assert capability["models"][0]["features"]["tool_calling"] is True
    assert capability["evidence"]["metadata"] is True
    denied = wire.get(url + "/local/v1/capabilities",
                      headers={"Authorization": "Bearer " + Path(token_b).read_text().strip()})
    assert denied.status_code == 200 and denied.json()["models"] == []

    with client_for(token) as client:
        model = client.models.list().data[0]
        assert model.id == "qwen3-4b" and model.created == 0
        result = client.chat.completions.create(model=model.id, messages=messages,
                                                n=1, max_completion_tokens=64, stop=["END"])
        assert result.choices[0].message.content == "SYNTHETIC_PRIVATE_OUTPUT"
        assert result.choices[0].finish_reason == "stop"
        with client.chat.completions.create(model=model.id, messages=messages,
                                             stream=True, stream_options={"include_usage": True}) as stream:
            chunks = list(stream)
        assert chunks[-1].choices == []
        assert chunks[-1].usage.total_tokens == 2
        assert chunks[-2].choices[0].finish_reason == "stop"

        first = client.chat.completions.create(model=model.id, messages=messages, tools=tools,
                                               tool_choice="required", parallel_tool_calls=False)
        assert first.choices[0].finish_reason == "tool_calls"
        call = first.choices[0].message.tool_calls[0]
        assert call.type == "function" and call.function.name == "lookup_demo"
        assert json.loads(call.function.arguments) == {"code": "TEST-001"}
        history = messages + [
            {"role": "assistant", "content": first.choices[0].message.content,
             "tool_calls": [{"id": call.id, "type": "function", "function": {
                 "name": call.function.name, "arguments": call.function.arguments}}]},
            {"role": "tool", "tool_call_id": call.id,
             "content": '{"status":"synthetic_record_found"}'},
        ]
        second = client.chat.completions.create(model=model.id, messages=history, tools=tools,
                                                tool_choice="none", parallel_tool_calls=False)
        assert second.choices[0].finish_reason == "stop"
        assert second.choices[0].message.content == "SYNTHETIC_PRIVATE_OUTPUT"

        # Function argument fragments are assembled only after the stream ends.
        with client.chat.completions.create(model=model.id, messages=messages, tools=tools,
                                             tool_choice="required", parallel_tool_calls=False,
                                             stream=True, stream_options={"include_usage": True}) as stream:
            chunks = list(stream)
        name, arguments = "", ""
        for chunk in chunks:
            if not chunk.choices:
                continue
            for part in chunk.choices[0].delta.tool_calls or []:
                assert part.index == 0
                if part.function:
                    name += part.function.name or ""
                    arguments += part.function.arguments or ""
        assert name == "lookup_demo" and json.loads(arguments) == {"code": "TEST-001"}
        assert chunks[-2].choices[0].finish_reason == "tool_calls"
        assert chunks[-1].usage.total_tokens == 2

        # The synthetic vLLM profile returns stop for a named function call.
        # The public API must normalize only the valid call to tool_calls.
        named = {"type": "function", "function": {"name": "lookup_demo"}}
        first = client.chat.completions.create(model=model.id, messages=messages, tools=tools,
                                               tool_choice=named, parallel_tool_calls=False)
        assert first.choices[0].finish_reason == "tool_calls"
        assert first.choices[0].message.tool_calls[0].function.name == "lookup_demo"
        with client.chat.completions.create(model=model.id, messages=messages, tools=tools,
                                             tool_choice=named, parallel_tool_calls=False,
                                             stream=True, stream_options={"include_usage": True}) as stream:
            chunks = list(stream)
        assert chunks[-2].choices[0].finish_reason == "tool_calls"
        assert chunks[-1].usage.total_tokens == 2

        for change, code in [({"n": 2}, 400), ({"model": "qwen3-8b"}, 403)]:
            try:
                client.chat.completions.create(**({"model": model.id, "messages": messages} | change))
                raise AssertionError("invalid request accepted")
            except APIStatusError as error:
                assert error.status_code == code

        raw = client.chat.completions.with_raw_response.create(
            model=model.id, messages=[{"role": "user", "content": "cancel"}], stream=True,
            extra_headers={"X-Apostille-Record": "metadata"},
        )
        run_id = raw.headers["X-Apostille-Run-ID"]
        with raw.parse() as stream:
            for _ in stream:
                break
        for _ in range(100):
            receipt = wire.get(url + "/local/v1/runs/" + run_id + "/evidence").json()
            if receipt["receipt_status"] == "failed":
                break
            time.sleep(.02)
        assert receipt["receipt_status"] == "failed", "cancelled run acquired a success receipt"


async def check_async():
    async with AsyncOpenAI(base_url=url + "/v1", api_key=token, max_retries=0, timeout=10,
                           http_client=httpx.AsyncClient(verify=tls, trust_env=False,
                                                          follow_redirects=False, timeout=10)) as client:
        result = await client.chat.completions.create(model="qwen3-4b", messages=messages)
        assert result.choices[0].finish_reason == "stop"
        async with await client.chat.completions.create(
            model="qwen3-4b", messages=messages, stream=True,
            stream_options={"include_usage": True},
        ) as stream:
            chunks = [chunk async for chunk in stream]
        assert chunks[-1].choices == [] and chunks[-1].usage.total_tokens == 2


asyncio.run(check_async())
for mode in ([], ["--tool-demo"]):
    demo = subprocess.run(
        [sys.executable, "../examples/openai_client.py", "--base-url", url,
         "--ca-file", ca_file, "--token-file", token_a, *mode],
        capture_output=True, text=True, timeout=20, check=False,
    )
    assert demo.returncode == 0, "customer integration example failed"
    assert demo.stdout == "Synthetic integration completed; no content was printed or saved.\n"
    assert demo.stderr == "", "customer example emitted diagnostics"
print("Official OpenAI client TLS contract passed: discovery, text, usage, tools, async, errors, cancellation")

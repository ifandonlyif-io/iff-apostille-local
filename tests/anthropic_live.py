"""Official Anthropic SDK against the actual TLS gateway and a synthetic runtime.

Invoked by integration_test.go. No Anthropic service is contacted. The client and
transport versions are test dependencies, independent of the native SDK.
"""
import asyncio
import logging
import os
from pathlib import Path
import ssl
import subprocess
import sys
import time

os.environ.pop("ANTHROPIC_LOG", None)
os.environ.pop("ANTHROPIC_CUSTOM_HEADERS", None)
logging.disable(logging.CRITICAL)

import anthropic
import httpx

url, ca_file, token_a, token_b = sys.argv[1:]
token = Path(token_a).read_text().strip()
tls = ssl.create_default_context(cafile=ca_file)
messages = [{"role": "user", "content": "SYNTHETIC_PRIVATE_INPUT"}]
tools = [{"name": "lookup_demo", "description": "Synthetic record lookup.", "strict": True,
          "input_schema": {"type": "object", "properties": {"code": {"type": "string"}},
                           "required": ["code"], "additionalProperties": False}}]
schema = {"type": "object", "properties": {"answer": {"type": "integer"}},
          "required": ["answer"], "additionalProperties": False}
OUTPUT = "SYNTHETIC_PRIVATE_OUTPUT"


def http_client():
    return anthropic.DefaultHttpxClient(verify=tls, trust_env=False,
                                        follow_redirects=False, timeout=10)


def client_for(**credentials):
    # Explicit credentials disable the SDK's environment and profile lookup.
    return anthropic.Anthropic(base_url=url, max_retries=0, http_client=http_client(), **credentials)


def only_tool_use(message):
    blocks = [b for b in message.content if b.type == "tool_use"]
    assert len(blocks) == 1 and message.stop_reason == "tool_use"
    assert blocks[0].name == "lookup_demo" and blocks[0].input == {"code": "TEST-001"}
    assert message.usage.input_tokens == 1 and message.usage.output_tokens == 1
    return blocks[0]


with httpx.Client(verify=tls, trust_env=False, follow_redirects=False,
                  timeout=10, headers={"Authorization": "Bearer " + token}) as wire:
    capability = wire.get(url + "/local/v1/capabilities").json()
    assert capability["api_family"] == "chat_completions" and capability["contract_version"] == "1"
    assert capability["compatible_apis"] == [{"family": "anthropic_messages", "path": "/v1/messages",
                                              "anthropic_version": "2023-06-01"}]
    model = capability["models"][0]["id"]

    with client_for(api_key=token) as client:
        result = client.messages.create(model=model, max_tokens=64, messages=messages)
        assert result.type == "message" and result.role == "assistant" and result.model == model
        assert result.stop_reason == "end_turn" and result.stop_sequence is None
        assert [b.type for b in result.content] == ["text"] and result.content[0].text == OUTPUT
        assert result.usage.input_tokens == 1 and result.usage.output_tokens == 1
        assert result.id.startswith("msg_")

        client.messages.create(model=model, max_tokens=64, system="SYNTHETIC_SYSTEM",
                               # anthropic 1.8.0 has no sampling parameters; the wire field is still accepted.
                               extra_body={"temperature": 0.2, "top_p": 0.9},
                               messages=[{"role": "user", "content": [{"type": "text", "text": "x"}]}])

        with client.messages.stream(model=model, max_tokens=64, messages=messages) as stream:
            text = "".join(stream.text_stream)
            final = stream.get_final_message()
        assert text == OUTPUT and final.content[0].text == OUTPUT
        assert final.stop_reason == "end_turn"
        assert final.usage.input_tokens == 1 and final.usage.output_tokens == 1

        for choice in ({"type": "any"}, {"type": "tool", "name": "lookup_demo"}):
            first = client.messages.create(model=model, max_tokens=64, messages=messages, tools=tools,
                                           tool_choice=choice | {"disable_parallel_tool_use": True})
            use = only_tool_use(first)
            with client.messages.stream(model=model, max_tokens=64, messages=messages, tools=tools,
                                        tool_choice=choice) as stream:
                streamed = stream.get_final_message()
            assert only_tool_use(streamed).input == {"code": "TEST-001"}

        history = messages + [
            {"role": "assistant", "content": [{"type": "tool_use", "id": use.id, "name": use.name, "input": use.input}]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": use.id,
                                          "content": '{"status":"synthetic_record_found"}'},
                                         {"type": "text", "text": "Summarise."}]},
        ]
        second = client.messages.create(model=model, max_tokens=64, messages=history, tools=tools,
                                        tool_choice={"type": "none"})
        assert second.stop_reason == "end_turn" and second.content[0].text == OUTPUT

        structured = client.messages.create(model=model, max_tokens=64, messages=messages,
                                            output_config={"format": {"type": "json_schema", "schema": schema}})
        assert structured.content[0].text == '{"answer":42}'

        for change, status, error in [
            ({"extra_body": {"top_k": 5}}, 400, anthropic.BadRequestError),
            ({"stop_sequences": ["END"]}, 400, anthropic.BadRequestError),
            ({"model": "qwen3-8b"}, 403, anthropic.PermissionDeniedError),
        ]:
            try:
                client.messages.create(**({"model": model, "max_tokens": 64, "messages": messages} | change))
                raise AssertionError("invalid request accepted")
            except error as failure:
                assert failure.status_code == status

        # A stream the gateway cannot validate must raise, never look complete.
        try:
            with client.messages.stream(model=model, max_tokens=64,
                                        messages=[{"role": "user", "content": "invalid-stream"}]) as stream:
                for _ in stream:
                    pass
            raise AssertionError("invalid runtime stream accepted")
        except anthropic.APIError:
            pass

        raw = client.messages.with_raw_response.create(
            model=model, max_tokens=64, messages=[{"role": "user", "content": "cancel"}], stream=True,
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

        recorded = client.messages.with_raw_response.create(
            model=model, max_tokens=64, messages=messages, extra_headers={"X-Apostille-Record": "metadata"})
        receipt = wire.get(url + "/local/v1/runs/" + recorded.headers["X-Apostille-Run-ID"] + "/evidence").json()
        assert receipt["receipt_status"] == "ready"
        assert recorded.parse().id == "msg_" + recorded.headers["X-Apostille-Run-ID"]

    # Bearer credentials are accepted as well; the SDK sends no X-Api-Key here.
    with client_for(auth_token=token) as client:
        assert client.messages.create(model=model, max_tokens=64, messages=messages).stop_reason == "end_turn"
    # Supplying both credentials is ambiguous and rejected.
    with client_for(api_key=token, auth_token=token) as client:
        try:
            client.messages.create(model=model, max_tokens=64, messages=messages)
            raise AssertionError("both credentials accepted")
        except anthropic.AuthenticationError:
            pass
    with client_for(api_key="synthetic-wrong-token-not-a-secret-xx") as client:
        try:
            client.messages.create(model=model, max_tokens=64, messages=messages)
            raise AssertionError("wrong credential accepted")
        except anthropic.AuthenticationError:
            pass


async def check_async():
    async with anthropic.AsyncAnthropic(
            base_url=url, api_key=token, max_retries=0,
            http_client=anthropic.DefaultAsyncHttpxClient(verify=tls, trust_env=False,
                                                          follow_redirects=False, timeout=10)) as client:
        result = await client.messages.create(model="qwen3-4b", max_tokens=64, messages=messages)
        assert result.stop_reason == "end_turn"
        async with client.messages.stream(model="qwen3-4b", max_tokens=64, messages=messages) as stream:
            final = await stream.get_final_message()
        assert final.stop_reason == "end_turn" and final.usage.output_tokens == 1


asyncio.run(check_async())
for mode in ([], ["--tool-demo"]):
    demo = subprocess.run(
        [sys.executable, "../examples/anthropic_client.py", "--base-url", url,
         "--ca-file", ca_file, "--token-file", token_a, *mode],
        capture_output=True, text=True, timeout=20, check=False,
    )
    assert demo.returncode == 0, "customer integration example failed"
    assert demo.stdout == "Synthetic integration completed; no content was printed or saved.\n"
    assert demo.stderr == "", "customer example emitted diagnostics"
print("Official Anthropic client TLS contract passed: text, stream, tools, structured output, errors, cancellation, async")

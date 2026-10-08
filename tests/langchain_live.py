"""Actual LangChain ChatOpenAI adapter against the TLS gateway.

Uses a synthetic local runtime; this checks wire interoperability, not GPU or
model quality. Never contacts OpenAI or LangSmith and never executes a tool.
"""
import asyncio
from importlib.metadata import version
import logging
import os
from pathlib import Path
import ssl
import sys
import time
from urllib.parse import urlsplit

for key in list(os.environ):
    if key.startswith(("LANGSMITH_", "LANGCHAIN_", "OPENAI_")):
        os.environ.pop(key)
os.environ["LANGSMITH_TRACING"] = "false"
os.environ["LANGCHAIN_TRACING_V2"] = "false"
logging.disable(logging.CRITICAL)

url, ca_file, token_a, token_b = sys.argv[1:]
target = urlsplit(url)
unexpected_network = []


def local_connections_only(event, args):
    # Catch accidental tracing, model metadata or tokenizer downloads as well
    # as cloud fallback. The single permitted endpoint is the test gateway.
    if event == "socket.connect":
        address = args[1]
        if not isinstance(address, tuple) or address[:2] != (target.hostname, target.port):
            unexpected_network.append(event)
            raise RuntimeError("external network access forbidden in integration test")
    if event == "socket.getaddrinfo" and args[0] != target.hostname:
        unexpected_network.append(event)
        raise RuntimeError("external DNS lookup forbidden in integration test")


sys.addaudithook(local_connections_only)

import httpx
from langchain_core.messages import AIMessageChunk, HumanMessage, ToolMessage
from langchain_openai import ChatOpenAI
from openai import APIStatusError

assert version("langchain-openai") == "1.1.11"
assert version("langchain-core") == "1.2.18"
token = Path(token_a).read_text().strip()
tls = ssl.create_default_context(cafile=ca_file)
messages = [HumanMessage(content="SYNTHETIC_PRIVATE_INPUT")]
tools = [{"type": "function", "function": {
    "name": "lookup_demo",
    "description": "Return a synthetic test record; this test does not execute it.",
    "parameters": {"type": "object", "properties": {"code": {"type": "string"}},
                   "required": ["code"], "additionalProperties": False},
}}]
schema = {
    "title": "Answer", "type": "object",
    "properties": {"answer": {"type": "integer"}},
    "required": ["answer"], "additionalProperties": False,
}
run_ids = []


def remember_run(response):
    if response.headers.get("X-Apostille-Run-ID"):
        run_ids.append(response.headers["X-Apostille-Run-ID"])


async def remember_async_run(response):
    remember_run(response)


def model_for(sync, asynchronous, key=token):
    return ChatOpenAI(
        model="qwen3-4b", base_url=url + "/v1", api_key=key,
        http_client=sync, http_async_client=asynchronous,
        max_retries=0, timeout=10, cache=False,
        use_responses_api=False, stream_usage=True,
    )


def assert_text(message):
    assert message.content == "SYNTHETIC_PRIVATE_OUTPUT"
    assert message.response_metadata["finish_reason"] == "stop"
    assert message.usage_metadata["total_tokens"] == 2


def assert_tool(message):
    assert message.response_metadata["finish_reason"] == "tool_calls"
    assert len(message.tool_calls) == 1
    call = message.tool_calls[0]
    assert call["id"] == "call_demo"
    assert call["name"] == "lookup_demo"
    assert call["args"] == {"code": "TEST-001"}
    assert message.usage_metadata["total_tokens"] == 2
    return call


def combine(chunks):
    assert chunks, "empty framework stream"
    result = chunks[0]
    assert isinstance(result, AIMessageChunk)
    for chunk in chunks[1:]:
        result = result + chunk
    return result


async def check():
    with httpx.Client(verify=tls, trust_env=False, follow_redirects=False,
                      timeout=10, event_hooks={"response": [remember_run]}) as sync:
        async with httpx.AsyncClient(verify=tls, trust_env=False,
                                     follow_redirects=False, timeout=10,
                                     event_hooks={"response": [remember_async_run]}) as asynchronous:
            llm = model_for(sync, asynchronous)
            assert_text(llm.invoke(messages, stop=["END"], max_completion_tokens=64))
            assert_text(await llm.ainvoke(messages))
            assert_text(combine(list(llm.stream(messages))))
            assert_text(combine([part async for part in llm.astream(messages)]))

            structured = llm.with_structured_output(schema, method="json_schema", strict=True)
            assert structured.invoke(messages) == {"answer": 42}
            assert await structured.ainvoke(messages) == {"answer": 42}
            assert list(structured.stream(messages))[-1] == {"answer": 42}

            for choice in ("required", "lookup_demo"):
                bound = llm.bind_tools(tools, strict=True, tool_choice=choice,
                                       parallel_tool_calls=False)
                first = bound.invoke(messages)
                call = assert_tool(first)
                result = ToolMessage(content="SYNTHETIC_PRIVATE_TOOL_RESULT",
                                     tool_call_id=call["id"], name=call["name"])
                # The customer supplies the tool result. No tool dispatcher or
                # function implementation is installed in the test or gateway.
                followup = llm.bind_tools(tools, strict=True, tool_choice="none",
                                          parallel_tool_calls=False)
                assert_text(followup.invoke(messages + [first, result]))
                assert_tool(combine(list(bound.stream(messages))))
                assert_tool(await bound.ainvoke(messages))
                assert_tool(combine([part async for part in bound.astream(messages)]))

            denied = model_for(sync, asynchronous, Path(token_b).read_text().strip())
            try:
                denied.invoke(messages)
                raise AssertionError("cross-project framework request accepted")
            except APIStatusError as error:
                assert error.status_code == 403

            # Explicit generator close cancels the underlying HTTP stream.
            # Read the run id from headers, independently of returned content.
            stream = llm.stream([HumanMessage(content="cancel")],
                                extra_headers={"X-Apostille-Record": "metadata"})
            next(stream)
            run_id = run_ids[-1]
            stream.close()
            for _ in range(100):
                evidence = sync.get(url + "/local/v1/runs/" + run_id + "/evidence",
                                    headers={"Authorization": "Bearer " + token}).json()
                if evidence["receipt_status"] == "failed":
                    break
                time.sleep(.02)
            assert evidence["receipt_status"] == "failed", "cancelled stream acquired a success receipt"

            stream = llm.astream([HumanMessage(content="cancel")],
                                 extra_headers={"X-Apostille-Record": "metadata"})
            await anext(stream)
            run_id = run_ids[-1]
            await stream.aclose()
            for _ in range(100):
                response = await asynchronous.get(url + "/local/v1/runs/" + run_id + "/evidence",
                                                  headers={"Authorization": "Bearer " + token})
                evidence = response.json()
                if evidence["receipt_status"] == "failed":
                    break
                await asyncio.sleep(.02)
            assert evidence["receipt_status"] == "failed", "cancelled async stream acquired a success receipt"


asyncio.run(check())
assert not unexpected_network, "framework attempted external network access"
print("LangChain TLS contract passed: text, schema, named/required tools, history, SSE usage, async, isolation, cancellation")

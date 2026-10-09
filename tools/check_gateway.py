"""Synthetic API acceptance against an operator-selected TLS gateway.

No model/runtime installation, retry, tool execution or content persistence.
The JSON report records check statuses only. A successful API check does not
identify the GPU used or qualify hardware, offline isolation or model quality.
"""
from __future__ import annotations

import argparse
import json
import logging
import sys
from collections.abc import Callable
from typing import Any

from apostille_local import Client


MESSAGES = [{"role": "user", "content": "Reply with a short synthetic greeting."}]
TOOLS = [{"type": "function", "function": {
    "name": "lookup_demo", "strict": True,
    "description": "Read an in-memory synthetic record, code TEST-001.",
    "parameters": {"type": "object", "properties": {"code": {"type": "string", "enum": ["TEST-001"]}},
                   "required": ["code"], "additionalProperties": False},
}}]
TOOL_MESSAGES = [{"role": "user", "content": "Call lookup_demo with code TEST-001."}]
FORMAT = {"type": "json_schema", "json_schema": {
    "name": "synthetic_answer", "strict": True,
    "schema": {"type": "object", "properties": {"answer": {"type": "integer", "const": 42}},
               "required": ["answer"], "additionalProperties": False},
}}
NAMED = {"type": "function", "function": {"name": "lookup_demo"}}


def require(condition: Any) -> None:
    # Explicit checks remain active with python -O, including imported accept().
    # Never include a payload, URL, key or backend exception in this error.
    if not condition:
        raise ValueError("invalid_api_contract")


def single_choice(result: Any) -> dict:
    choices = result["choices"]
    require(isinstance(choices, list) and len(choices) == 1)
    choice = choices[0]
    require(type(choice["index"]) is int and choice["index"] == 0)
    return choice


def checked_json(raw: Any) -> Any:
    require(isinstance(raw, str))

    def unique(pairs: list) -> dict:
        result: dict = {}
        for key, value in pairs:
            require(key not in result)
            result[key] = value
        return result

    return json.loads(raw, object_pairs_hook=unique)


def checked_usage(usage: Any) -> None:
    require(isinstance(usage, dict))
    for field in ("prompt_tokens", "completion_tokens", "total_tokens"):
        require(type(usage[field]) is int and 0 <= usage[field] <= 1 << 32)
    require(usage["total_tokens"] == usage["prompt_tokens"] + usage["completion_tokens"])


def text_completed(result: Any) -> None:
    choice = single_choice(result)
    require(choice["finish_reason"] == "stop")
    require(choice["message"]["role"] == "assistant")
    require(isinstance(choice["message"]["content"], str) and choice["message"]["content"].strip())
    require(choice["message"].get("tool_calls") in (None, []))
    if result.get("usage") is not None:
        checked_usage(result["usage"])


def checked_call(result: Any) -> dict:
    choice = single_choice(result)
    require(choice["finish_reason"] == "tool_calls")
    require(choice["message"]["role"] == "assistant")
    calls = choice["message"]["tool_calls"]
    require(isinstance(calls, list) and len(calls) == 1)
    call = calls[0]
    require(call["type"] == "function" and isinstance(call["id"], str) and 0 < len(call["id"]) <= 128)
    require(call["function"]["name"] == "lookup_demo")
    require(checked_json(call["function"]["arguments"]) == {"code": "TEST-001"})
    if result.get("usage") is not None:
        checked_usage(result["usage"])
    return call


def completed_stream(stream: Any, *, tools: bool) -> dict:
    content, call_id, name, arguments, kind = "", "", "", "", ""
    reason, usage = None, None
    for event in stream:
        require(isinstance(event["choices"], list))
        if not event["choices"]:
            require(reason is not None and usage is None)
            checked_usage(event["usage"])
            usage = event["usage"]
            continue
        require(reason is None and usage is None)
        choice = single_choice(event)
        delta = choice["delta"]
        require(isinstance(delta, dict) and delta.get("role") in (None, "assistant"))
        text = delta.get("content")
        require(text is None or isinstance(text, str))
        content += text or ""
        require(len(content) <= 2 << 20)
        fragments = delta.get("tool_calls")
        require(fragments is None or isinstance(fragments, list))
        require(tools or not fragments)
        for fragment in fragments or []:
            require(type(fragment["index"]) is int and fragment["index"] == 0)
            function = fragment.get("function") or {}
            require(isinstance(function, dict))
            values = [fragment.get("id"), fragment.get("type"), function.get("name"), function.get("arguments")]
            require(all(value is None or isinstance(value, str) for value in values))
            call_id += values[0] or ""
            if values[1]:
                require(values[1] == "function" and kind in ("", "function"))
                kind = values[1]
            name += values[2] or ""
            arguments += values[3] or ""
            require(len(call_id) <= 128 and len(name) <= 64 and len(arguments) <= 64 << 10)
        reason = choice.get("finish_reason")
        require(reason is None or isinstance(reason, str))
        if event.get("usage") is not None:
            checked_usage(event["usage"])
    require(usage is not None)
    message = {"role": "assistant", "content": content}
    if tools:
        message["tool_calls"] = [{"id": call_id, "type": kind,
                                  "function": {"name": name, "arguments": arguments}}]
    return {"choices": [{"index": 0, "finish_reason": reason, "message": message}], "usage": usage}


def accept(client: Any, *, vendor: str, model: str, require_tools: bool = False) -> dict:
    if vendor not in ("amd", "nvidia"):
        raise ValueError("invalid_vendor")
    report: dict[str, Any] = {
        "schema": "apostille-local-api-check/1", "requested_vendor": vendor,
        "result": "failed", "checks": {}, "hardware_acceptance": "unverified",
        "gpu_identity": "not_observed", "offline_isolation": "not_tested",
        "recovery": "not_tested", "evidence": "not_tested",
    }
    checks = report["checks"]

    def check(name: str, action: Callable[[], Any]) -> bool:
        try:
            action()
        except Exception:
            # A client or model may include sensitive payloads in exception text.
            # Only static, locally selected check names/statuses reach the report.
            checks[name] = "failed"
            return False
        checks[name] = "passed"
        return True

    selected: dict = {}

    def discovery() -> None:
        capability = client.capabilities.get()
        require(capability["contract_version"] == "1")
        require(capability["tool_execution"] == "client")
        items = [m for m in capability["models"] if m["id"] == model]
        require(len(items) == 1)
        selected.update(items[0])
        require(any(m["id"] == model for m in client.models.list()["data"]))
        require(type(selected["max_output_tokens"]) is int and selected["max_output_tokens"] > 0)
        require(all(selected["features"][f] is True for f in ("text", "streaming", "stream_usage", "json_schema")))
        require(type(selected["features"]["tool_calling"]) is bool)

    if not check("discovery", discovery):
        return report
    limit = min(256, selected["max_output_tokens"])
    common = {"model": model, "max_completion_tokens": limit}
    check("text", lambda: text_completed(client.chat.create(messages=MESSAGES, **common)))

    def text_stream() -> None:
        with client.chat.stream(messages=MESSAGES, stream_options={"include_usage": True}, **common) as stream:
            text_completed(completed_stream(stream, tools=False))

    check("stream_usage", text_stream)

    def structured() -> None:
        result = client.chat.create(messages=[{"role": "user", "content": "Return a JSON object with answer equal to 42."}],
                                    response_format=FORMAT, **common)
        text_completed(result)
        require(checked_json(result["choices"][0]["message"]["content"]) == {"answer": 42})

    check("json_schema", structured)
    tool_checks = ("named_tool", "required_tool", "tool_roundtrip", "named_tool_stream")
    if not selected["features"]["tool_calling"]:
        checks.update({name: "failed" if require_tools else "not_enabled" for name in tool_checks})
    else:
        tool_args = dict(common, tools=TOOLS, parallel_tool_calls=False)
        check("named_tool", lambda: checked_call(client.chat.create(messages=TOOL_MESSAGES, tool_choice=NAMED, **tool_args)))
        check("required_tool", lambda: checked_call(client.chat.create(messages=TOOL_MESSAGES, tool_choice="required", **tool_args)))

        def roundtrip() -> None:
            first = client.chat.create(messages=TOOL_MESSAGES, tool_choice=NAMED, **tool_args)
            call = checked_call(first)
            # Synthetic result only: no dynamic function invocation or tool I/O.
            history = TOOL_MESSAGES + [
                {"role": "assistant", "content": None, "tool_calls": [call]},
                {"role": "tool", "tool_call_id": call["id"], "content": '{"status":"synthetic_record_found"}'},
            ]
            text_completed(client.chat.create(messages=history, tool_choice="none", **tool_args))

        check("tool_roundtrip", roundtrip)

        def tool_stream() -> None:
            with client.chat.stream(messages=TOOL_MESSAGES, tool_choice=NAMED,
                                    stream_options={"include_usage": True}, **tool_args) as stream:
                checked_call(completed_stream(stream, tools=True))

        check("named_tool_stream", tool_stream)
    report["result"] = "passed" if all(value in ("passed", "not_enabled") for value in checks.values()) else "failed"
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True, help="HTTPS gateway origin, without /v1")
    parser.add_argument("--token-file", required=True)
    parser.add_argument("--ca-file", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--vendor", choices=("amd", "nvidia"), required=True,
                        help="operator-declared deployment; this command does not inspect GPU identity")
    parser.add_argument("--require-tools", action="store_true")
    args = parser.parse_args()
    logging.disable(logging.CRITICAL)
    try:
        with Client(base_url=args.base_url, token_file=args.token_file, ca_file=args.ca_file, timeout=600) as client:
            report = accept(client, vendor=args.vendor, model=args.model, require_tools=args.require_tools)
    except Exception:
        report = {"schema": "apostille-local-api-check/1", "requested_vendor": args.vendor,
                  "result": "failed", "checks": {"configuration": "failed"},
                  "hardware_acceptance": "unverified", "gpu_identity": "not_observed"}
    print(json.dumps(report, sort_keys=True))
    return 0 if report["result"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())

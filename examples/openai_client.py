"""Synthetic customer-side integration; openai==2.29.0, httpx==0.28.1.

No prompt, answer, tool arguments or token is printed or persisted. The optional
tool is an in-memory synthetic lookup, not an agent executor or a shell bridge.
"""
import argparse
import json
import logging
import os
from pathlib import Path
import ssl
import sys
from urllib.parse import urlsplit

# The SDK can enable content-bearing debug logs during import. This example has
# no logging sinks. Customer applications must enforce their own logging policy.
os.environ.pop("OPENAI_LOG", None)
logging.disable(logging.CRITICAL)

import httpx
from openai import OpenAI


TOOLS = [{"type": "function", "function": {
    "name": "lookup_demo",
    "description": "Read a synthetic demonstration record by its fixed code.",
    "strict": True,
    "parameters": {"type": "object", "properties": {"code": {"type": "string"}},
                   "required": ["code"], "additionalProperties": False},
}}]


def settings(base_url, token_file, ca_file):
    origin = urlsplit(base_url)
    if (origin.scheme != "https" or not origin.hostname or origin.username is not None
            or origin.password is not None or origin.query or origin.fragment
            or origin.path not in ("", "/") or origin.port == 0):
        raise ValueError("https_gateway_origin_required")
    with Path(token_file).open("rb") as source:
        raw = source.read(4097)
    if len(raw) > 4096:
        raise ValueError("invalid_token_file")
    token = raw.decode("ascii").strip()
    if not 32 <= len(token) <= 256 or any(ord(c) < 33 or ord(c) > 126 for c in token):
        raise ValueError("invalid_token_file")
    return base_url.rstrip("/"), token, ssl.create_default_context(cafile=ca_file)


def run(base_url, token_file, ca_file, tool_demo=False):
    origin, token, tls = settings(base_url, token_file, ca_file)
    transport = httpx.Client(verify=tls, trust_env=False, follow_redirects=False,
                             timeout=600.0)
    with OpenAI(base_url=origin + "/v1", api_key=token, max_retries=0,
                timeout=600.0, http_client=transport) as client:
        response = transport.get(origin + "/local/v1/capabilities",
                                 headers={"Authorization": "Bearer " + token})
        response.raise_for_status()
        capability = response.json()
        if capability["contract_version"] != "1" or not capability["models"]:
            raise ValueError("supported_ready_model_required")
        model = capability["models"][0]
        limit = min(128, model["max_output_tokens"])
        messages = [{"role": "user", "content": "Reply with a short greeting."}]
        extra = {}
        if tool_demo:
            if not model["features"]["tool_calling"]:
                raise ValueError("tool_calling_not_enabled")
            messages = [{"role": "user", "content": "Look up synthetic record TEST-001 using lookup_demo."}]
            first = client.chat.completions.create(
                model=model["id"], messages=messages, tools=TOOLS,
                tool_choice={"type": "function", "function": {"name": "lookup_demo"}},
                parallel_tool_calls=False, max_completion_tokens=limit,
            )
            choice = first.choices[0]
            calls = choice.message.tool_calls
            if choice.finish_reason != "tool_calls" or not calls or len(calls) != 1:
                raise ValueError("expected_demo_tool_call")
            call = calls[0]
            # Independent application-level checks before executing any tool.
            # Do not use arbitrary function names, eval, shell commands or URLs.
            if (call.type != "function" or call.function.name != "lookup_demo"
                    or json.loads(call.function.arguments) != {"code": "TEST-001"}):
                raise ValueError("unapproved_demo_tool_call")
            messages.extend([
                {"role": "assistant", "content": choice.message.content,
                 "tool_calls": [{"id": call.id, "type": "function", "function": {
                     "name": call.function.name, "arguments": call.function.arguments}}]},
                {"role": "tool", "tool_call_id": call.id,
                 "content": json.dumps({"status": "synthetic_record_found"})},
            ])
            # Keep the definition for validation of history; request final text.
            extra = {"tools": TOOLS, "tool_choice": "none", "parallel_tool_calls": False}
        result = client.chat.completions.create(
            model=model["id"], messages=messages, n=1,
            max_completion_tokens=limit, **extra,
        )
        if result.choices[0].finish_reason != "stop":
            raise ValueError("completion_not_finished")
        # Consume result inside the approved customer application. Deliberately
        # avoid repr(result), exception bodies, tracing callbacks and disk writes.
    print("Synthetic integration completed; no content was printed or saved.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", required=True, help="HTTPS gateway origin without /v1")
    parser.add_argument("--token-file", required=True)
    parser.add_argument("--ca-file", required=True)
    parser.add_argument("--tool-demo", action="store_true")
    args = parser.parse_args()
    try:
        run(args.base_url, args.token_file, args.ca_file, args.tool_demo)
    except Exception:
        # SDK/transport exception text may contain request or response content.
        print("Integration failed; check gateway capability, access and model configuration.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

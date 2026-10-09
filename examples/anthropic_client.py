"""Synthetic customer-side integration; anthropic==1.8.0.

No prompt, answer, tool arguments or token is printed or persisted. The optional
tool is an in-memory synthetic lookup, not an agent executor or a shell bridge.
The gateway serves its own local model; nothing here contacts Anthropic.
"""
import argparse
import logging
import os
from pathlib import Path
import ssl
import sys
from urllib.parse import urlsplit

# The SDK can enable content-bearing debug logs and extra headers from the
# environment. This example has no logging sinks and sends only what it sets.
os.environ.pop("ANTHROPIC_LOG", None)
os.environ.pop("ANTHROPIC_CUSTOM_HEADERS", None)
logging.disable(logging.CRITICAL)

import anthropic


TOOLS = [{
    "name": "lookup_demo",
    "description": "Read a synthetic demonstration record by its fixed code.",
    "strict": True,
    "input_schema": {"type": "object", "properties": {"code": {"type": "string"}},
                     "required": ["code"], "additionalProperties": False},
}]


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
    if not 32 <= len(token) <= 249 or any(ord(c) < 33 or ord(c) > 126 for c in token):
        raise ValueError("invalid_token_file")
    return base_url.rstrip("/"), token, ssl.create_default_context(cafile=ca_file)


def run(base_url, token_file, ca_file, tool_demo=False):
    origin, token, tls = settings(base_url, token_file, ca_file)
    transport = anthropic.DefaultHttpxClient(verify=tls, trust_env=False,
                                             follow_redirects=False, timeout=600.0)
    # The Anthropic base URL has no /v1: the SDK appends /v1/messages itself.
    # An explicit api_key (sent as X-Api-Key) is the project token, never an
    # Anthropic key. It also stops the SDK from reading credentials elsewhere.
    with anthropic.Anthropic(base_url=origin, api_key=token, max_retries=0,
                             http_client=transport) as client:
        # Reuse the SDK's own httpx2 transport; anthropic does not install httpx.
        response = transport.get(origin + "/local/v1/capabilities",
                                 headers={"Authorization": "Bearer " + token})
        response.raise_for_status()
        capability = response.json()
        if (capability["contract_version"] != "1" or not capability["models"]
                or not any(api["family"] == "anthropic_messages"
                           for api in capability.get("compatible_apis", []))):
            raise ValueError("supported_ready_model_required")
        model = capability["models"][0]
        limit = min(128, model["max_output_tokens"])
        messages = [{"role": "user", "content": "Reply with a short greeting."}]
        extra = {}
        if tool_demo:
            if not model["features"]["tool_calling"]:
                raise ValueError("tool_calling_not_enabled")
            messages = [{"role": "user", "content": "Look up synthetic record TEST-001 using lookup_demo."}]
            first = client.messages.create(
                model=model["id"], max_tokens=limit, messages=messages, tools=TOOLS,
                tool_choice={"type": "tool", "name": "lookup_demo", "disable_parallel_tool_use": True},
            )
            calls = [block for block in first.content if block.type == "tool_use"]
            if first.stop_reason != "tool_use" or len(calls) != 1:
                raise ValueError("expected_demo_tool_call")
            call = calls[0]
            # Independent application-level checks before executing any tool.
            # Do not use arbitrary function names, eval, shell commands or URLs.
            if call.name != "lookup_demo" or call.input != {"code": "TEST-001"}:
                raise ValueError("unapproved_demo_tool_call")
            messages.extend([
                {"role": "assistant", "content": [
                    {"type": "tool_use", "id": call.id, "name": call.name, "input": call.input}]},
                {"role": "user", "content": [
                    {"type": "tool_result", "tool_use_id": call.id,
                     "content": '{"status":"synthetic_record_found"}'}]},
            ])
            # Keep the definition for validation of history; request final text.
            extra = {"tools": TOOLS, "tool_choice": {"type": "none"}}
        result = client.messages.create(
            model=model["id"], max_tokens=limit, messages=messages, **extra,
        )
        if result.stop_reason != "end_turn":
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

import asyncio
import base64
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import httpx
import certifi

from apostille_local import APIError, Client, ConfigurationError
from apostille_local.client import _SSE
from client_test_helpers import AsyncBody, Fixture, SyncBody

MESSAGES = [{"role": "user", "content": "synthetic test"}]
CHUNK = b'data: {"choices":[{"delta":{"content":"synthetic"}}]}\n\n'
DONE = b"data: [DONE]\n\n"
MANIFEST = b'{ "synthetic": true, "order": [2, 1] }\n'
BUNDLE = b'{"synthetic_bundle":true}\n'
TOOLS = [{"type": "function", "function": {
    "name": "lookup_synthetic", "description": "Lookup a synthetic identifier",
    "parameters": {"type": "object", "properties": {"id": {"type": "string"}},
                   "required": ["id"], "additionalProperties": False}, "strict": True}}]
CALL = {"id": "call-1", "type": "function",
        "function": {"name": "lookup_synthetic", "arguments": '{"id":"synthetic-1"}'}}
HISTORY = MESSAGES + [{"role": "assistant", "content": None, "tool_calls": [CALL]},
                      {"role": "tool", "content": "synthetic-result", "tool_call_id": "call-1"}]


def tool_events():
    chunks = [
        {"choices": [{"index": 0, "delta": {"role": "assistant", "tool_calls": [
            {"index": 0, "id": "call-1", "type": "function",
             "function": {"name": "lookup_synthetic", "arguments": '{"id":'}}]}, "finish_reason": None}]},
        {"choices": [{"index": 0, "delta": {"tool_calls": [
            {"index": 0, "function": {"arguments": '"synthetic-1"}'}}]}, "finish_reason": None}]},
        {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
        {"choices": [], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}},
    ]
    return chunks, [b"data: " + json.dumps(c).encode() + b"\n\n" for c in chunks] + [DONE]


def ready_evidence():
    return {"receipt_status": "ready", "manifest": {"synthetic": "object is not exported"},
            "bundle": {"synthetic_bundle": "object is not exported"},
            "manifest_base64": base64.b64encode(MANIFEST).decode(),
            "bundle_base64": base64.b64encode(BUNDLE).decode()}


class SyncTests(Fixture, unittest.TestCase):
    def test_routes_headers_and_response(self):
        seen = []

        def handle(request):
            seen.append(request)
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-secret")
            return httpx.Response(200, headers={"X-Apostille-Run-ID": "run-123"},
                                  json={"data": [], "choices": [], "receipt_status": "pending"})

        with self.client(handle) as client:
            self.assertEqual(client.models.list()["data"], [])
            result = client.chat.create(model="synthetic", messages=MESSAGES, record=True)
            self.assertEqual(result.run_id, "run-123")
            self.assertEqual(result.to_dict()["choices"], [])
            self.assertNotIn("choices", repr(result))
            self.assertEqual(client.evidence.get(result.run_id)["receipt_status"], "pending")
        self.assertEqual([r.url.path for r in seen], ["/v1/models", "/v1/chat/completions", "/local/v1/runs/run-123/evidence"])
        self.assertEqual(seen[1].headers["X-Apostille-Record"], "metadata")
        self.assertNotIn("X-Apostille-Record", seen[0].headers)
        self.assertNotIn("n", json.loads(seen[1].content))

    def test_tls_and_url_boundaries(self):
        rejected = ["http://gateway.test", "http://127.0.0.1", "https://u:p@gateway.test", "https://gateway.test/v1", "https://gateway.test/?key=secret", "https://gateway.test/#x", "https://gateway.test:0", "https://gateway.test:wrong"]
        for url in rejected:
            with self.subTest(url=url), self.assertRaises(ConfigurationError):
                Client(base_url=url, token_file=self.token)
        for url in ["http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080"]:
            with Client(base_url=url, token_file=self.token, allow_insecure=True):
                pass
        for url in ["http://10.0.0.1", "http://192.168.1.1", "http://gateway.test"]:
            with self.assertRaises(ConfigurationError):
                Client(base_url=url, token_file=self.token, allow_insecure=True)

    def test_token_validation_sanitized(self):
        for secret in ["", "secret\r\nHeader: value", "a" * 4097, "secret token"]:
            self.token.write_text(secret)
            with self.assertRaisesRegex(ConfigurationError, "^invalid_token_file$"):
                self.client(lambda r: httpx.Response(200))

    def test_explicit_ca_bundle_and_invalid_ca(self):
        with Client(base_url="https://gateway.test", token_file=self.token,
                    ca_file=certifi.where()) as client:
            context = client._http._transport._pool._ssl_context
            self.assertTrue(context.check_hostname)
            self.assertGreater(len(context.get_ca_certs()), 0)
        with self.assertRaisesRegex(ConfigurationError, "^invalid_ca_file$"):
            Client(base_url="https://gateway.test", token_file=self.token, ca_file=self.token)

    def test_no_redirect_retries_or_error_body(self):
        seen = []
        body = SyncBody([b"backend-secret"])

        def handle(request):
            seen.append(request)
            return httpx.Response(307, headers={"location": "https://elsewhere.test"}, stream=body)

        with self.client(handle) as client, self.assertRaises(APIError) as caught:
            client.models.list()
        self.assertEqual(len(seen), 1)
        self.assertEqual(body.reads, 0)
        self.assertTrue(body.closed)
        self.assertEqual(str(caught.exception), "http_error")
        self.assertEqual(caught.exception.status_code, 307)

    def test_environment_proxy_ignored(self):
        with patch.dict("os.environ", {"HTTPS_PROXY": "http://127.0.0.1:9"}):
            with self.client(lambda r: httpx.Response(200, json={})) as client:
                self.assertFalse(client._http.trust_env)
                client.models.list()

    def test_transport_exception_sanitized(self):
        seen = []

        def handle(request):
            seen.append(request)
            raise httpx.ConnectError("synthetic-secret backend-secret", request=request)

        with self.client(handle) as client, self.assertRaises(APIError) as caught:
            client.models.list()
        self.assertEqual(str(caught.exception), "transport_error")
        self.assertTrue(caught.exception.__suppress_context__)
        self.assertEqual(len(seen), 1)

    def test_stream_delivers_before_eof_and_early_exit_closes(self):
        body = SyncBody([CHUNK, CHUNK, DONE])
        with self.client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream", "X-Apostille-Run-ID": "run-1"}, stream=body)) as client:
            with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, "run-1")
                self.assertEqual(next(iter(stream))["choices"][0]["delta"]["content"], "synthetic")
                self.assertEqual(body.reads, 1)
            self.assertTrue(body.closed)

    def test_stream_multiline_crlf_utf8_and_done(self):
        raw = ': comment\r\ndata: {"choices":\r\ndata: [{"delta":{"content":"測試"}}]}\r\n\r\ndata: [DONE]\r\n\r\n'.encode()
        parser = _SSE("run-1")
        result = []
        for b in raw:
            result.extend(parser.feed(bytes([b])))
        self.assertEqual(result[0]["choices"][0]["delta"]["content"], "測試")
        self.assertTrue(parser.done)

    def test_stream_size_cap_before_unbounded_line(self):
        parser = _SSE(None)
        with patch("apostille_local.client._MAX_EVENT", 32), self.assertRaisesRegex(APIError, "stream_event_too_large"):
            list(parser.feed(b"data: " + b"a" * 33))

    def test_stream_reads_to_end_after_done(self):
        # The response ends only after the gateway settles the receipt.
        body = SyncBody([CHUNK, DONE, b"\r\n", b"\n"])
        with self.client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
            with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(len(list(stream)), 1)
                self.assertEqual(body.reads, 4)
        # Data after [DONE], or more blank bytes than one event may hold, is rejected.
        for blocks, limit in [([CHUNK, DONE, CHUNK], 1024), ([DONE, b"\n" * 33], 32)]:
            body = SyncBody(blocks)
            with self.subTest(limit=limit), patch("apostille_local.client._MAX_EVENT", limit), \
                    self.client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
                with self.assertRaisesRegex(APIError, "^invalid_stream$"):
                    with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                        list(stream)

    def test_stream_incomplete_invalid_and_error(self):
        for raw, code in [(CHUNK, "incomplete_stream"), (b"data: bad\n\n", "invalid_response"), (b'data: {"error":{"message":"backend-secret"}}\n\n', "stream_error")]:
            body = SyncBody([raw])
            with self.subTest(code=code), self.client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
                with self.assertRaisesRegex(APIError, "^" + code + "$"):
                    with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                        list(stream)
                self.assertTrue(body.closed)

    def test_request_boundaries(self):
        invalid = [{"temperature": float("nan")}, {"max_tokens": True}, {"top_p": 2}, {"top_p": 0}, {"response_format": {"type": "json_object"}}, {"messages": [{"role": "tool", "content": "x"}]}, {"messages": [{"role": "user", "content": []}]}, {"messages": [{"role": "user", "content": ""}]}]
        with self.client(lambda r: self.fail("invalid request reached transport")) as client:
            for options in invalid:
                args = {"model": "synthetic", "messages": MESSAGES} | options
                with self.subTest(options=options), self.assertRaises(ConfigurationError):
                    client.chat.create(**args)
            with self.assertRaises(ConfigurationError):
                client.chat.create(model="synthetic", messages=MESSAGES, tools=[{}])
            with self.assertRaises(ConfigurationError):
                client.chat.create(model="synthetic", messages=MESSAGES, n=2)
            with self.assertRaises(ConfigurationError):
                client.evidence.get("../other-project")
            with self.assertRaisesRegex(ConfigurationError, "^invalid_request$"):
                client.chat.create(model="synthetic", messages=[{"role": "user", "content": chr(0xD800)}])

    def test_json_response_cap(self):
        with self.client(lambda r: httpx.Response(200, content=b"{}" * 10)) as client:
            with patch("apostille_local.client._MAX_RESPONSE", 8), self.assertRaisesRegex(APIError, "response_too_large"):
                client.models.list()

    def test_structured_array_root_reaches_gateway(self):
        def handle(request):
            schema = json.loads(request.content)["response_format"]["json_schema"]["schema"]
            self.assertEqual(schema["type"], "array")
            return httpx.Response(200, json={"choices": []})
        with self.client(handle) as client:
            client.chat.create(model="synthetic", messages=MESSAGES,
                               response_format={"type": "json_schema", "json_schema": {
                                   "name": "items", "strict": True,
                                   "schema": {"type": "array", "items": {"type": "string"}}}})

    def test_evidence_download_exact_bytes_and_modes(self):
        directory = Path(self.directory.name) / "download"
        with self.client(lambda r: httpx.Response(200, json=ready_evidence())) as client:
            paths = client.evidence.download("run-1", directory)
        self.assertEqual(paths, {"manifest": directory / "manifest.json", "bundle": directory / "bundle.json"})
        self.assertEqual(paths["manifest"].read_bytes(), MANIFEST)
        self.assertEqual(paths["bundle"].read_bytes(), BUNDLE)
        self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
        for path in paths.values():
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_evidence_download_refuses_existing_directory(self):
        directory = Path(self.directory.name) / "existing"
        directory.mkdir()
        sentinel = directory / "manifest.json"
        sentinel.write_bytes(b"preserve")
        with self.client(lambda r: httpx.Response(200, json=ready_evidence())) as client:
            with self.assertRaisesRegex(ConfigurationError, "^download_directory_exists$"):
                client.evidence.download("run-1", directory)
        self.assertEqual(sentinel.read_bytes(), b"preserve")
        self.assertEqual(list(directory.iterdir()), [sentinel])

    def test_evidence_download_rejects_nonready_and_invalid_base64(self):
        records = [(ready_evidence() | {"receipt_status": status}, "evidence_not_ready")
                   for status in ("pending", "failed")]
        records.extend((ready_evidence() | {"manifest_base64": value}, "invalid_evidence")
                       for value in (None, "", "!invalid!", "Zh==", "Zg==\n", "測試"))
        records.append(({"receipt_status": "ready", "manifest": {}, "bundle": {}}, "invalid_evidence"))
        directory = Path(self.directory.name) / "rejected"
        for record, code in records:
            with self.subTest(record=record), self.client(lambda r: httpx.Response(200, json=record)) as client:
                with self.assertRaisesRegex(APIError, "^" + code + "$"):
                    client.evidence.download("run-1", directory)
            self.assertFalse(directory.exists())

    def test_evidence_download_cleans_partial_write_failure(self):
        directory = Path(self.directory.name) / "partial"
        with self.client(lambda r: httpx.Response(200, json=ready_evidence())) as client:
            with patch("apostille_local.client.os.fsync", side_effect=[None, OSError("synthetic-secret")]):
                with self.assertRaisesRegex(ConfigurationError, "^evidence_write_failed$"):
                    client.evidence.download("run-1", directory)
        self.assertFalse(directory.exists())

    def test_evidence_download_missing_parent_is_sanitized(self):
        directory = Path(self.directory.name) / "missing" / "download"
        with self.client(lambda r: httpx.Response(200, json=ready_evidence())) as client:
            with self.assertRaisesRegex(ConfigurationError, "^download_directory_unavailable$"):
                client.evidence.download("run-1", directory)
        self.assertFalse(directory.exists())


class AsyncTests(Fixture, unittest.IsolatedAsyncioTestCase):
    async def test_async_evidence_download_exact_bytes(self):
        directory = Path(self.directory.name) / "async-download"
        async with self.async_client(lambda r: httpx.Response(200, json=ready_evidence())) as client:
            paths = await client.evidence.download("run-1", directory)
        self.assertEqual(paths["manifest"].read_bytes(), MANIFEST)
        self.assertEqual(paths["bundle"].read_bytes(), BUNDLE)
        for path in paths.values():
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    async def test_async_routes_and_stream(self):
        def handle(request):
            if request.url.path == "/v1/chat/completions" and json.loads(request.content)["stream"]:
                return httpx.Response(200, headers={"content-type": "text/event-stream", "X-Apostille-Run-ID": "run-1"}, stream=AsyncBody([CHUNK, DONE]))
            return httpx.Response(200, json={"data": [], "receipt_status": "ready", "choices": []})

        async with self.async_client(handle) as client:
            self.assertEqual((await client.models.list())["data"], [])
            self.assertEqual((await client.chat.create(model="synthetic", messages=MESSAGES))["choices"], [])
            self.assertEqual((await client.evidence.get("run-1"))["receipt_status"], "ready")
            async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, "run-1")
                self.assertEqual(len([chunk async for chunk in stream]), 1)

    async def test_async_stream_reads_to_end_after_done(self):
        for blocks, error in [([CHUNK, DONE, b"\n"], None), ([CHUNK, DONE, CHUNK], "invalid_stream")]:
            body = AsyncBody(blocks)
            async with self.async_client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
                async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                    if error is None:
                        self.assertEqual(len([chunk async for chunk in stream]), 1)
                    else:
                        with self.assertRaisesRegex(APIError, "^" + error + "$"):
                            [chunk async for chunk in stream]

    async def test_async_cancellation_closes_response(self):
        body = AsyncBody([CHUNK], wait=True)
        async with self.async_client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
            async def consume():
                async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                    async for _ in stream:
                        pass
            task = asyncio.create_task(consume())
            await asyncio.wait_for(body.waiting.wait(), 2)
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await task
            self.assertTrue(body.closed)

    async def test_async_early_break_closes(self):
        body = AsyncBody([CHUNK], wait=True)
        async with self.async_client(lambda r: httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)) as client:
            async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                async for _ in stream:
                    break
            self.assertTrue(body.closed)

    async def test_async_http_errors_no_retry_or_content(self):
        requests = []
        body = AsyncBody([b"backend-secret"])

        def handle(request):
            requests.append(request)
            return httpx.Response(502, stream=body)

        async with self.async_client(handle) as client:
            with self.assertRaisesRegex(APIError, "^backend_error$"):
                await client.models.list()
        self.assertEqual(len(requests), 1)
        self.assertTrue(body.closed)


class InteroperabilityTests(Fixture, unittest.TestCase):
    def test_tool_round_trip_is_caller_owned(self):
        seen = []

        def handle(request):
            seen.append(json.loads(request.content))
            if len(seen) == 1:
                return httpx.Response(200, json={"choices": [{"message": {
                    "role": "assistant", "content": None, "tool_calls": [CALL]}, "finish_reason": "tool_calls"}]})
            return httpx.Response(200, json={"choices": [{"message": {
                "role": "assistant", "content": "synthetic-final"}, "finish_reason": "stop"}]})

        with self.client(handle) as client:
            result = client.chat.create(model="synthetic", messages=MESSAGES, tools=TOOLS,
                                        tool_choice="required", parallel_tool_calls=False,
                                        max_completion_tokens=32, n=1, stop=["END"])
            self.assertEqual(len(seen), 1)  # The SDK never executes or replies to a tool call.
            self.assertEqual(result["choices"][0]["message"]["tool_calls"], [CALL])
            final = client.chat.create(model="synthetic", messages=HISTORY, tools=TOOLS,
                                       tool_choice="none", max_tokens=32)
            self.assertEqual(final["choices"][0]["message"]["content"], "synthetic-final")
        self.assertEqual(seen[0]["tools"], TOOLS)
        self.assertEqual(seen[0]["tool_choice"], "required")
        self.assertIs(seen[0]["parallel_tool_calls"], False)
        self.assertEqual(seen[0]["max_completion_tokens"], 32)
        self.assertNotIn("max_tokens", seen[0])
        self.assertEqual(seen[0]["stop"], ["END"])
        self.assertEqual(seen[0]["n"], 1)
        self.assertEqual(seen[1]["messages"], HISTORY)

    def test_capabilities_authenticated_and_stream_usage_preserved(self):
        chunks, events = tool_events()
        seen = []
        body = SyncBody(events)

        def handle(request):
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-secret")
            seen.append(request)
            if request.url.path == "/local/v1/capabilities":
                return httpx.Response(200, json={"models": []})
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)

        with self.client(handle) as client:
            self.assertEqual(client.capabilities.get()["models"], [])
            with client.chat.stream(model="synthetic", messages=MESSAGES, tools=TOOLS,
                                    tool_choice={"type": "function", "function": {"name": "lookup_synthetic"}},
                                    stream_options={"include_usage": True}, n=1) as stream:
                self.assertEqual([c.to_dict() for c in stream], chunks)
        request = json.loads(seen[1].content)
        self.assertEqual(request["stream_options"], {"include_usage": True})
        self.assertEqual(request["tool_choice"]["function"]["name"], "lookup_synthetic")
        self.assertTrue(body.closed)

    def test_empty_tools_omitted_and_assistant_content_optional(self):
        seen = []
        history = [*HISTORY]
        history[1] = {"role": "assistant", "tool_calls": [CALL]}
        history.append({"role": "assistant", "content": "synthetic-final", "tool_calls": []})
        with self.client(lambda r: seen.append(json.loads(r.content)) or httpx.Response(200, json={})) as client:
            client.chat.create(model="synthetic", messages=history, tools=[], tool_choice="none")
        self.assertNotIn("tools", seen[0])
        self.assertNotIn("content", seen[0]["messages"][1])
        self.assertEqual(seen[0]["messages"][-1]["tool_calls"], [])

    def test_invalid_common_options_never_reach_transport(self):
        bad = [
            {"n": True}, {"n": 2}, {"stop": []}, {"stop": ""}, {"stop": ["x"] * 5},
            {"temperature": 10 ** 1000}, {"top_p": 10 ** 1000},
            {"stop": "測" * 342}, {"stop": True}, {"max_completion_tokens": 0},
            {"max_completion_tokens": 1, "max_tokens": 1}, {"stream_options": {"include_usage": True}},
            {"tool_choice": "auto"}, {"tool_choice": "required", "tools": []},
            {"tool_choice": {"type": "function", "function": {"name": "missing"}}, "tools": TOOLS},
            {"parallel_tool_calls": False}, {"parallel_tool_calls": 1, "tools": TOOLS},
            {"tools": [{"type": "function", "function": {"name": "bad name", "parameters": {"type": "object"}}}]},
            {"tools": [{"type": "function", "function": {"name": "a", "parameters": {"$ref": "https://elsewhere"}}}]},
            {"tools": TOOLS * 2}, {"tools": TOOLS, "response_format": {"type": "json_schema", "json_schema": {
                "name": "result", "strict": True, "schema": {"type": "object"}}}},
        ]
        with self.client(lambda r: self.fail("invalid request reached transport")) as client:
            for options in bad:
                with self.subTest(options=options), self.assertRaises(ConfigurationError):
                    client.chat.create(model="synthetic", messages=MESSAGES, **options)
            for options in ({}, {"include_usage": 1}, {"include_usage": True, "extra": 1}):
                with self.subTest(options=options), self.assertRaises(ConfigurationError):
                    with client.chat.stream(model="synthetic", messages=MESSAGES, stream_options=options):
                        pass
            with self.assertRaises(TypeError):
                client.chat.create(model="synthetic", messages=MESSAGES, external_endpoint="https://elsewhere")

    def test_tool_history_validation(self):
        invalid = [
            HISTORY[:-1],
            MESSAGES + [{"role": "tool", "content": "x", "tool_call_id": "missing"}],
            HISTORY[:-1] + [MESSAGES[0]], HISTORY + [HISTORY[-1]],
            HISTORY + HISTORY[1:],
            MESSAGES + [{"role": "assistant", "content": None}],
            MESSAGES + [{"role": "assistant", "content": None, "tool_calls": []}],
            MESSAGES + [{"role": "assistant", "content": "text", "tool_calls": None}],
            MESSAGES + [{"role": "assistant", "content": "text", "refusal": None, "function_call": None}],
            MESSAGES + [{"role": "assistant", "tool_calls": [CALL | {"id": "bad id"}]}],
        ]
        for arguments in ('{"id":1,"id":2}', '[]', '{"id":NaN}', '{"id":"' + chr(92) + 'uD800"}', "x" * 65537):
            call = CALL | {"function": CALL["function"] | {"arguments": arguments}}
            invalid.append(MESSAGES + [{"role": "assistant", "tool_calls": [call]}, HISTORY[-1]])
        with self.client(lambda r: self.fail("invalid history reached transport")) as client:
            for history in invalid:
                with self.subTest(history=history), self.assertRaises(ConfigurationError):
                    client.chat.create(model="synthetic", messages=history)

    def test_malformed_nested_tool_objects_are_sanitized(self):
        calls = [None, {}, CALL | {"function": None}, CALL | {"id": []},
                 CALL | {"type": None}, CALL | {"function": {"name": None, "arguments": "{}"}},
                 CALL | {"function": {"name": "lookup_synthetic", "arguments": {}}},
                 CALL | {"function": {"name": "lookup_synthetic", "arguments": '{"a":1,"a":2}'}}]
        tools = [None, {}, TOOLS[0] | {"function": None},
                 {"type": "function", "function": {"name": [], "parameters": {}}},
                 {"type": "function", "function": {"name": "lookup_synthetic", "parameters": None}}]
        with self.client(lambda r: self.fail("malformed object reached transport")) as client:
            for call in calls:
                with self.subTest(call=call), self.assertRaisesRegex(ConfigurationError, "^invalid_messages$"):
                    client.chat.create(model="synthetic", messages=MESSAGES + [
                        {"role": "assistant", "tool_calls": [call]}, HISTORY[-1]])
            for tool in tools:
                with self.subTest(tool=tool), self.assertRaisesRegex(ConfigurationError, "^invalid_tools$"):
                    client.chat.create(model="synthetic", messages=MESSAGES, tools=[tool])

    def test_request_and_message_bounds(self):
        with self.client(lambda r: self.fail("oversize request reached transport")) as client:
            with self.assertRaises(ConfigurationError):
                client.chat.create(model="synthetic", messages=MESSAGES * 129)
            with patch("apostille_local.client._MAX_REQUEST", 64), self.assertRaisesRegex(ConfigurationError, "request_too_large"):
                client.chat.create(model="synthetic", messages=MESSAGES)

    def test_only_known_status_specific_codes_preserved(self):
        scenarios = [
            (409, {"error": {"code": "model_inactive", "message": "synthetic-secret"}}, "model_inactive"),
            (429, {"error": {"code": "project_capacity_exceeded", "message": "synthetic-secret"}}, "project_capacity_exceeded"),
            (503, {"error": {"code": "draining", "message": "synthetic-secret"}}, "draining"),
            (400, {"error": {"code": "tool_calling_unavailable", "message": "synthetic-secret"}}, "tool_calling_unavailable"),
            (502, {"error": {"code": "synthetic-secret", "message": "synthetic-secret"}}, "backend_error"),
            (502, {"error": {"code": "model_inactive"}}, "backend_error"),
            (502, {"error": "synthetic-secret"}, "backend_error"),
            (502, ["synthetic-secret"], "backend_error"),
        ]
        for status, data, code in scenarios:
            with self.subTest(code=code), self.client(lambda r: httpx.Response(status, json=data,
                    headers={"X-Apostille-Run-ID": "run-1"})) as client:
                with self.assertRaises(APIError) as caught:
                    client.models.list()
                self.assertEqual(caught.exception.code, code)
                self.assertEqual(caught.exception.status_code, status)
                self.assertEqual(caught.exception.run_id, "run-1")
                self.assertNotIn("synthetic-secret", str(caught.exception))

    def test_error_read_is_bounded(self):
        body = SyncBody([b"x" * 4096] * 100)
        with self.client(lambda r: httpx.Response(502, headers={"content-type": "application/json"}, stream=body)) as client:
            with self.assertRaisesRegex(APIError, "^backend_error$"):
                client.models.list()
        self.assertLess(body.reads, 5)
        self.assertTrue(body.closed)


class AsyncInteroperabilityTests(Fixture, unittest.IsolatedAsyncioTestCase):
    async def test_capabilities_options_and_tool_stream(self):
        chunks, events = tool_events()
        seen = []
        body = AsyncBody(events)

        def handle(request):
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-secret")
            seen.append(request)
            if request.url.path == "/local/v1/capabilities":
                return httpx.Response(200, json={"models": []})
            if json.loads(request.content)["stream"]:
                return httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=body)
            return httpx.Response(200, json={"choices": []})

        async with self.async_client(handle) as client:
            self.assertEqual((await client.capabilities.get())["models"], [])
            await client.chat.create(model="synthetic", messages=HISTORY, tools=TOOLS,
                                     tool_choice="none", parallel_tool_calls=False,
                                     stop="END", max_completion_tokens=32, n=1)
            async with client.chat.stream(model="synthetic", messages=MESSAGES, tools=TOOLS,
                                          tool_choice="auto", parallel_tool_calls=False,
                                          stream_options={"include_usage": True}, n=1) as stream:
                self.assertEqual([c.to_dict() async for c in stream], chunks)
        payload = json.loads(seen[1].content)
        self.assertEqual(payload["messages"], HISTORY)
        self.assertEqual(payload["max_completion_tokens"], 32)
        self.assertIs(payload["parallel_tool_calls"], False)
        self.assertEqual(payload["stop"], "END")
        self.assertTrue(body.closed)

    async def test_stream_http_error_code_sanitized_and_no_retry(self):
        seen = []

        def handle(request):
            seen.append(request)
            return httpx.Response(429, json={"error": {"code": "capacity_exceeded", "message": "synthetic-secret"}})

        async with self.async_client(handle) as client:
            with self.assertRaisesRegex(APIError, "^capacity_exceeded$"):
                async with client.chat.stream(model="synthetic", messages=MESSAGES):
                    self.fail("error yielded a stream")
        self.assertEqual(len(seen), 1)

    async def test_create_http_error_code_sanitized(self):
        async with self.async_client(lambda r: httpx.Response(409, json={"error": {
                "code": "model_inactive", "message": "synthetic-secret"}})) as client:
            with self.assertRaisesRegex(APIError, "^model_inactive$"):
                await client.chat.create(model="synthetic", messages=MESSAGES)

    async def test_invalid_options_rejected_before_transport(self):
        async with self.async_client(lambda r: self.fail("invalid request reached transport")) as client:
            with self.assertRaises(ConfigurationError):
                await client.chat.create(model="synthetic", messages=MESSAGES, n=2)
            with self.assertRaises(ConfigurationError):
                async with client.chat.stream(model="synthetic", messages=HISTORY[:-1], tools=TOOLS):
                    pass


if __name__ == "__main__":
    unittest.main()

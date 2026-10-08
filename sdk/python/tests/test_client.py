import asyncio
import base64
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import httpx
import certifi

from apostille_local import APIError, AsyncClient, Client, ConfigurationError
from apostille_local.client import _SSE

MESSAGES = [{"role": "user", "content": "synthetic test"}]
CHUNK = b'data: {"choices":[{"delta":{"content":"synthetic"}}]}\n\n'
DONE = b"data: [DONE]\n\n"
MANIFEST = b'{ "synthetic": true, "order": [2, 1] }\n'
BUNDLE = b'{"synthetic_bundle":true}\n'


def ready_evidence():
    return {"receipt_status": "ready", "manifest": {"synthetic": "object is not exported"},
            "bundle": {"synthetic_bundle": "object is not exported"},
            "manifest_base64": base64.b64encode(MANIFEST).decode(),
            "bundle_base64": base64.b64encode(BUNDLE).decode()}


class SyncBody(httpx.SyncByteStream):
    def __init__(self, blocks):
        self.blocks = blocks
        self.closed = False
        self.reads = 0

    def __iter__(self):
        for block in self.blocks:
            self.reads += 1
            yield block

    def close(self):
        self.closed = True


class AsyncBody(httpx.AsyncByteStream):
    def __init__(self, blocks, wait=False):
        self.blocks = blocks
        self.wait = wait
        self.waiting = asyncio.Event()
        self.closed = False

    async def __aiter__(self):
        for block in self.blocks:
            yield block
        if self.wait:
            self.waiting.set()
            await asyncio.Event().wait()

    async def aclose(self):
        self.closed = True


class Fixture:
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.token = Path(self.directory.name) / "token"
        self.token.write_text("synthetic-secret\n")

    def client(self, handler, **kwargs):
        return Client(base_url="https://gateway.test", token_file=self.token,
                      transport=httpx.MockTransport(handler), **kwargs)

    def async_client(self, handler):
        return AsyncClient(base_url="https://gateway.test", token_file=self.token,
                           transport=httpx.MockTransport(handler))


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
            with self.assertRaises(TypeError):
                client.chat.create(model="synthetic", messages=MESSAGES, tools=[])
            with self.assertRaises(TypeError):
                client.chat.create(model="synthetic", messages=MESSAGES, n=1)
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


if __name__ == "__main__":
    unittest.main()

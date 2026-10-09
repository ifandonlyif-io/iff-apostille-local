"""Terminal SSE validation must be independent of HTTP chunk boundaries."""

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import httpx

from apostille_local import APIError, AsyncClient, Client


MESSAGES = [{"role": "user", "content": "synthetic test"}]
CHUNK = b'data: {"choices":[]}\n\n'
RUN_ID = "synthetic-run"
TAIL_LIMIT = 64
TERMINATORS = {
    "lf": b"data: [DONE]\n\n",
    "crlf": b"data: [DONE]\r\n\r\n",
    "cr": b"data: [DONE]\r\r",
}


def chunkings(terminal, tail):
    """Partition identical bytes around the terminator and across its tail."""
    yield "same_chunk", [terminal + tail]
    yield "after_terminal", [terminal, tail]
    yield "before_last_delimiter_byte", [terminal[:-1], terminal[-1:] + tail]
    yield "before_last_delimiter_pair", [terminal[:-2], terminal[-2:] + tail]
    yield "after_first_tail_byte", [terminal + tail[:1], tail[1:]]
    middle = len(tail) // 2
    yield "tail_in_three_chunks", [terminal + tail[:middle], tail[middle:-1], tail[-1:]]
    yield "bytewise", [bytes([b]) for b in terminal + tail]


def invalid_tail_cases():
    tails = {
        "error_event": b'data: {"error":{"message":"synthetic"}}\n\n',
        "junk": b"synthetic-junk",
        "space": b" ",
    }
    for ending, terminal in TERMINATORS.items():
        for name, tail in tails.items():
            for partition, blocks in chunkings(terminal, tail):
                yield (ending, name, partition), blocks


def blank_tail_cases():
    for ending, terminal in TERMINATORS.items():
        for name, tail in {"empty": b"", "mixed": b"\r\n\r\n\n\r"}.items():
            for partition, blocks in chunkings(terminal, tail):
                yield (ending, name, partition), blocks


def tail_limit_cases():
    # Starting the tail with CR avoids combining a standalone CR terminator
    # with a following LF. A CRLF terminator's own final LF is not tail data.
    for ending, terminal in TERMINATORS.items():
        for length in (TAIL_LIMIT, TAIL_LIMIT + 1):
            tail = (b"\r\n" * (TAIL_LIMIT // 2) + b"\r")[:length]
            for partition, blocks in chunkings(terminal, tail):
                yield (ending, length, partition), blocks, length > TAIL_LIMIT


class SyncBody(httpx.SyncByteStream):
    def __init__(self, blocks):
        self.blocks = [CHUNK, *blocks]
        self.reads = 0
        self.exhausted = False
        self.closed = False

    def __iter__(self):
        for block in self.blocks:
            self.reads += 1
            yield block
        self.exhausted = True

    def close(self):
        self.closed = True


class AsyncBody(httpx.AsyncByteStream):
    def __init__(self, blocks):
        self.blocks = [CHUNK, *blocks]
        self.reads = 0
        self.exhausted = False
        self.closed = False

    async def __aiter__(self):
        for block in self.blocks:
            self.reads += 1
            yield block
        self.exhausted = True

    async def aclose(self):
        self.closed = True


class Fixture:
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.token = Path(directory.name) / "token"
        self.token.write_text("synthetic-secret\n")

    def options(self, body):
        return {
            "base_url": "https://gateway.test",
            "token_file": self.token,
            "transport": httpx.MockTransport(lambda request: httpx.Response(
                200,
                headers={"content-type": "text/event-stream", "X-Apostille-Run-ID": RUN_ID},
                stream=body,
            )),
        }

    def assert_complete(self, chunks, body):
        self.assertEqual(len(chunks), 1)
        self.assertEqual(chunks[0]["choices"], [])
        self.assertTrue(body.exhausted, "iterator returned without consuming HTTP EOF")
        self.assertEqual(body.reads, len(body.blocks))


class SyncStreamTailTests(Fixture, unittest.TestCase):
    def check_stream(self, blocks, invalid=False):
        body = SyncBody(blocks)
        with Client(**self.options(body)) as client:
            with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, RUN_ID)
                if invalid:
                    with self.assertRaises(APIError) as caught:
                        list(stream)
                    self.assertEqual(caught.exception.code, "invalid_stream")
                    self.assertEqual(caught.exception.run_id, RUN_ID)
                else:
                    self.assert_complete(list(stream), body)
        self.assertTrue(body.closed)

    def test_rejects_nonblank_tail_for_all_chunk_boundaries(self):
        for case, blocks in invalid_tail_cases():
            with self.subTest(case=case):
                self.check_stream(blocks, invalid=True)

    def test_blank_tail_consumes_http_eof(self):
        for case, blocks in blank_tail_cases():
            with self.subTest(case=case):
                self.check_stream(blocks)

    def test_tail_limit_counts_same_and_later_chunks(self):
        with patch("apostille_local.client._MAX_EVENT", TAIL_LIMIT):
            for case, blocks, invalid in tail_limit_cases():
                with self.subTest(case=case):
                    self.check_stream(blocks, invalid)


class AsyncStreamTailTests(Fixture, unittest.IsolatedAsyncioTestCase):
    async def check_stream(self, blocks, invalid=False):
        body = AsyncBody(blocks)
        async with AsyncClient(**self.options(body)) as client:
            async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, RUN_ID)
                if invalid:
                    with self.assertRaises(APIError) as caught:
                        [chunk async for chunk in stream]
                    self.assertEqual(caught.exception.code, "invalid_stream")
                    self.assertEqual(caught.exception.run_id, RUN_ID)
                else:
                    chunks = [chunk async for chunk in stream]
                    self.assert_complete(chunks, body)
        self.assertTrue(body.closed)

    async def test_rejects_nonblank_tail_for_all_chunk_boundaries(self):
        for case, blocks in invalid_tail_cases():
            with self.subTest(case=case):
                await self.check_stream(blocks, invalid=True)

    async def test_blank_tail_consumes_http_eof(self):
        for case, blocks in blank_tail_cases():
            with self.subTest(case=case):
                await self.check_stream(blocks)

    async def test_tail_limit_counts_same_and_later_chunks(self):
        with patch("apostille_local.client._MAX_EVENT", TAIL_LIMIT):
            for case, blocks, invalid in tail_limit_cases():
                with self.subTest(case=case):
                    await self.check_stream(blocks, invalid)

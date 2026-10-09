"""Terminal SSE validation must be independent of HTTP chunk boundaries."""

import json
import unittest
from unittest.mock import patch

import httpx

from apostille_local import APIError
from client_test_helpers import AsyncBody, Fixture, SyncBody


MESSAGES = [{"role": "user", "content": "synthetic test"}]
CHUNK = b'data: {"choices":[]}\n\n'
RUN_ID = "synthetic-run"
TAIL_LIMIT = 64
TERMINATORS = {
    "lf": b"data: [DONE]\n\n",
    "crlf": b"data: [DONE]\r\n\r\n",
    "cr": b"data: [DONE]\r\r",
    # The final LF completes the standalone CR's separator, including when
    # transport blocks split those bytes; it does not consume the tail budget.
    "cr_followed_by_lf": b"data: [DONE]\r\r\n",
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


def coalesced_event_cases():
    data = {"choices": [{"delta": {"content": "synthetic"}}]}
    usage = {"choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
    tails = {
        "blank": (b"\r\n", False),
        "junk": (b"synthetic-junk", True),
        "error": (b'data: {"error":{"message":"synthetic"}}\n\n', True),
    }
    for name, events in {"data": [data], "usage": [usage], "data_and_usage": [data, usage]}.items():
        prefix = b"".join(b"data: " + json.dumps(event).encode() + b"\n\n" for event in events)
        for ending, terminal in TERMINATORS.items():
            for tail_name, (tail, invalid) in tails.items():
                yield (name, ending, tail_name), [prefix + terminal + tail], invalid, events


def response(body):
    return httpx.Response(200, headers={"content-type": "text/event-stream", "X-Apostille-Run-ID": RUN_ID},
                          stream=body)


class TailFixture(Fixture):
    def assert_complete(self, chunks, body, expected):
        self.assertEqual([chunk.to_dict() for chunk in chunks], expected)
        self.assertTrue(body.exhausted, "iterator returned without consuming HTTP EOF")
        self.assertEqual(body.reads, len(body.blocks))


class SyncStreamTailTests(TailFixture, unittest.TestCase):
    def check_stream(self, blocks, invalid=False, expected=None):
        body = SyncBody(blocks)
        with self.client(lambda request: response(body)) as client:
            with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, RUN_ID)
                if invalid:
                    with self.assertRaises(APIError) as caught:
                        list(stream)
                    self.assertEqual(caught.exception.code, "invalid_stream")
                    self.assertEqual(caught.exception.run_id, RUN_ID)
                else:
                    self.assert_complete(list(stream), body, expected if expected is not None else [{"choices": []}])
        self.assertTrue(body.closed)

    def test_rejects_nonblank_tail_for_all_chunk_boundaries(self):
        for case, blocks in invalid_tail_cases():
            with self.subTest(case=case):
                self.check_stream([CHUNK, *blocks], invalid=True)

    def test_blank_tail_consumes_http_eof(self):
        for case, blocks in blank_tail_cases():
            with self.subTest(case=case):
                self.check_stream([CHUNK, *blocks])

    def test_tail_limit_counts_same_and_later_chunks(self):
        with patch("apostille_local.client._MAX_EVENT", TAIL_LIMIT):
            for case, blocks, invalid in tail_limit_cases():
                with self.subTest(case=case):
                    self.check_stream([CHUNK, *blocks], invalid)

    def test_events_done_and_tail_in_one_transport_block(self):
        for case, blocks, invalid, expected in coalesced_event_cases():
            with self.subTest(case=case):
                self.check_stream(blocks, invalid, expected)


class AsyncStreamTailTests(TailFixture, unittest.IsolatedAsyncioTestCase):
    async def check_stream(self, blocks, invalid=False, expected=None):
        body = AsyncBody(blocks)
        async with self.async_client(lambda request: response(body)) as client:
            async with client.chat.stream(model="synthetic", messages=MESSAGES) as stream:
                self.assertEqual(stream.run_id, RUN_ID)
                if invalid:
                    with self.assertRaises(APIError) as caught:
                        [chunk async for chunk in stream]
                    self.assertEqual(caught.exception.code, "invalid_stream")
                    self.assertEqual(caught.exception.run_id, RUN_ID)
                else:
                    chunks = [chunk async for chunk in stream]
                    self.assert_complete(chunks, body, expected if expected is not None else [{"choices": []}])
        self.assertTrue(body.closed)

    async def test_rejects_nonblank_tail_for_all_chunk_boundaries(self):
        for case, blocks in invalid_tail_cases():
            with self.subTest(case=case):
                await self.check_stream([CHUNK, *blocks], invalid=True)

    async def test_blank_tail_consumes_http_eof(self):
        for case, blocks in blank_tail_cases():
            with self.subTest(case=case):
                await self.check_stream([CHUNK, *blocks])

    async def test_tail_limit_counts_same_and_later_chunks(self):
        with patch("apostille_local.client._MAX_EVENT", TAIL_LIMIT):
            for case, blocks, invalid in tail_limit_cases():
                with self.subTest(case=case):
                    await self.check_stream([CHUNK, *blocks], invalid)

    async def test_events_done_and_tail_in_one_transport_block(self):
        for case, blocks, invalid, expected in coalesced_event_cases():
            with self.subTest(case=case):
                await self.check_stream(blocks, invalid, expected)

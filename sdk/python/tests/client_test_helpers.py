"""Synthetic HTTP streams and client setup shared by SDK tests."""

import asyncio
from pathlib import Path
import tempfile

import httpx

from apostille_local import AsyncClient, Client


class SyncBody(httpx.SyncByteStream):
    def __init__(self, blocks):
        self.blocks = blocks
        self.closed = False
        self.reads = 0
        self.exhausted = False

    def __iter__(self):
        for block in self.blocks:
            self.reads += 1
            yield block
        self.exhausted = True

    def close(self):
        self.closed = True


class AsyncBody(httpx.AsyncByteStream):
    def __init__(self, blocks, wait=False):
        self.blocks = blocks
        self.wait = wait
        self.waiting = asyncio.Event()
        self.closed = False
        self.reads = 0
        self.exhausted = False

    async def __aiter__(self):
        for block in self.blocks:
            self.reads += 1
            yield block
        if self.wait:
            self.waiting.set()
            await asyncio.Event().wait()
        self.exhausted = True

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

    def async_client(self, handler, **kwargs):
        return AsyncClient(base_url="https://gateway.test", token_file=self.token,
                           transport=httpx.MockTransport(handler), **kwargs)

"""Small synchronous/asynchronous client; never logs request or response content."""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator, Mapping
from contextlib import asynccontextmanager, contextmanager
import base64
import binascii
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import ssl
from typing import Any
from urllib.parse import urlsplit

import httpx

_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
_MAX_RESPONSE = 4 * 1024 * 1024
_MAX_EVENT = 1024 * 1024


class ConfigurationError(ValueError):
    """A local configuration or request is invalid; never includes secret values."""


class APIError(Exception):
    """Sanitized error message; underlying transport exception display is suppressed."""

    def __init__(self, code: str, *, status_code: int | None = None,
                 run_id: str | None = None):
        self.code = code
        self.status_code = status_code
        self.run_id = run_id
        super().__init__(code)


class Response(Mapping[str, Any]):
    """A JSON object plus its X-Apostille-Run-ID, if provided."""

    def __init__(self, data: dict[str, Any], run_id: str | None):
        self._data = data
        self.run_id = run_id

    def __getitem__(self, key: str) -> Any:
        return self._data[key]

    def __iter__(self) -> Iterator[str]:
        return iter(self._data)

    def __len__(self) -> int:
        return len(self._data)

    def to_dict(self) -> dict[str, Any]:
        return dict(self._data)

    def __repr__(self) -> str:
        return f"Response(run_id={self.run_id!r})"


def _base_url(value: str, allow_insecure: bool) -> str:
    try:
        u = urlsplit(value)
        if not u.hostname or u.username is not None or u.password is not None:
            raise ValueError
        if u.query or u.fragment or u.path not in ("", "/") or u.port == 0:
            raise ValueError
        if u.scheme == "http" and allow_insecure:
            try:
                local = ipaddress.ip_address(u.hostname).is_loopback
            except ValueError:
                local = u.hostname == "localhost"
            if not local:
                raise ValueError
        elif u.scheme != "https":
            raise ValueError
        httpx.URL(value)
    except (ValueError, httpx.InvalidURL):
        raise ConfigurationError("invalid_base_url") from None
    return value.rstrip("/")


def _token(path: str | Path) -> str:
    try:
        with open(path, "rb") as f:
            raw = f.read(4097)
        if len(raw) > 4096:
            raise ValueError
        value = raw.strip().decode("ascii")
        if not value or any(ord(c) <= 32 or ord(c) >= 127 for c in value):
            raise ValueError
    except (OSError, ValueError):
        raise ConfigurationError("invalid_token_file") from None
    return value


def _tls(ca_file: str | Path | None) -> ssl.SSLContext | bool:
    if ca_file is None:
        return True
    try:
        return ssl.create_default_context(cafile=str(ca_file))
    except (OSError, ValueError):
        raise ConfigurationError("invalid_ca_file") from None


def _run_id(response: httpx.Response) -> str | None:
    value = response.headers.get("X-Apostille-Run-ID", "")
    return value if _ID.fullmatch(value) else None


def _check_status(response: httpx.Response) -> None:
    if not 200 <= response.status_code < 300:
        codes = {400: "invalid_request", 401: "unauthorized", 403: "forbidden",
                 404: "not_found", 409: "conflict", 413: "request_too_large",
                 415: "unsupported_media_type", 429: "rate_limited",
                 502: "backend_error", 503: "unavailable", 504: "timeout"}
        raise APIError(codes.get(response.status_code, "http_error"),
                       status_code=response.status_code, run_id=_run_id(response))


def _json(data: bytes, run_id: str | None) -> Response:
    try:
        value = json.loads(data)
        if not isinstance(value, dict):
            raise ValueError
    except (ValueError, UnicodeError):
        raise APIError("invalid_response", run_id=run_id) from None
    if "error" in value:
        raise APIError("backend_error", run_id=run_id)
    return Response(value, run_id)


def _payload(model: str, messages: list[dict[str, str]], *, stream: bool,
             temperature: float | None = None, top_p: float | None = None,
             max_tokens: int | None = None, response_format: dict | None = None) -> dict[str, Any]:
    if not isinstance(model, str) or not model or not isinstance(messages, list) or not messages:
        raise ConfigurationError("invalid_request")
    for m in messages:
        if not isinstance(m, dict) or set(m) != {"role", "content"}:
            raise ConfigurationError("invalid_messages")
        if m["role"] not in ("system", "user", "assistant") or not isinstance(m["content"], str) or not m["content"]:
            raise ConfigurationError("invalid_messages")
    payload: dict[str, Any] = {"model": model, "messages": messages, "stream": stream}
    for name, value, upper in (("temperature", temperature, 2), ("top_p", top_p, 1)):
        if value is not None:
            if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value) or not 0 <= value <= upper:
                raise ConfigurationError("invalid_sampling_parameter")
            if name == "top_p" and value == 0:
                raise ConfigurationError("invalid_sampling_parameter")
            payload[name] = value
    if max_tokens is not None:
        if type(max_tokens) is not int or max_tokens < 1:
            raise ConfigurationError("invalid_max_tokens")
        payload["max_tokens"] = max_tokens
    if response_format is not None:
        if not isinstance(response_format, dict) or set(response_format) != {"type", "json_schema"} or response_format["type"] != "json_schema":
            raise ConfigurationError("invalid_response_format")
        definition = response_format["json_schema"]
        if not isinstance(definition, dict) or set(definition) != {"name", "strict", "schema"} or not isinstance(definition["name"], str) or not definition["name"] or definition["strict"] is not True:
            raise ConfigurationError("invalid_response_format")
        if not isinstance(definition["schema"], dict):
            raise ConfigurationError("invalid_response_format")
        payload["response_format"] = response_format
    try:
        json.dumps(payload, allow_nan=False, ensure_ascii=False).encode("utf-8")
    except (TypeError, ValueError, UnicodeError, RecursionError):
        raise ConfigurationError("invalid_request") from None
    return payload


class _SSE:
    """Bound memory before decoding; handle LF, CRLF, CR and multiline data."""

    def __init__(self, run_id: str | None):
        self.run_id = run_id
        self.line = bytearray()
        self.data: list[bytes] = []
        self.size = 0
        self.cr = False
        self.done = False

    def feed(self, block: bytes) -> Iterator[Response]:
        for b in block:
            if self.done:
                return
            if b == 10 and self.cr:
                self.cr = False
                continue
            self.cr = b == 13
            self.size += 1
            if self.size > _MAX_EVENT:
                raise APIError("stream_event_too_large", run_id=self.run_id)
            if b not in (10, 13):
                self.line.append(b)
                continue
            line = bytes(self.line)
            self.line.clear()
            if line:
                field, _, value = line.partition(b":")
                if field == b"data":
                    self.data.append(value[1:] if value.startswith(b" ") else value)
                continue
            payload = b"\n".join(self.data)
            self.data.clear()
            self.size = 0
            if not payload:
                continue
            if payload == b"[DONE]":
                self.done = True
                return
            try:
                result = _json(payload, self.run_id)
            except APIError as exc:
                if exc.code == "backend_error":
                    raise APIError("stream_error", run_id=self.run_id) from None
                raise
            if not isinstance(result.get("choices"), list):
                raise APIError("invalid_stream", run_id=self.run_id)
            yield result


class Stream:
    """Use only inside Client.chat.stream's context manager."""

    def __init__(self, response: httpx.Response):
        self._response = response
        self.run_id = _run_id(response)

    def __iter__(self) -> Iterator[Response]:
        parser = _SSE(self.run_id)
        try:
            for block in self._response.iter_bytes():
                yield from parser.feed(block)
                if parser.done:
                    return
        except httpx.TimeoutException:
            raise APIError("timeout", run_id=self.run_id) from None
        except httpx.HTTPError:
            raise APIError("transport_error", run_id=self.run_id) from None
        if not parser.done:
            raise APIError("incomplete_stream", run_id=self.run_id)


class AsyncStream:
    """Use only inside AsyncClient.chat.stream's async context manager."""

    def __init__(self, response: httpx.Response):
        self._response = response
        self.run_id = _run_id(response)

    async def __aiter__(self) -> AsyncIterator[Response]:
        parser = _SSE(self.run_id)
        try:
            async for block in self._response.aiter_bytes():
                for result in parser.feed(block):
                    yield result
                if parser.done:
                    return
        except httpx.TimeoutException:
            raise APIError("timeout", run_id=self.run_id) from None
        except httpx.HTTPError:
            raise APIError("transport_error", run_id=self.run_id) from None
        if not parser.done:
            raise APIError("incomplete_stream", run_id=self.run_id)


def _headers(record: bool) -> dict[str, str]:
    if type(record) is not bool:
        raise ConfigurationError("invalid_record_option")
    return {"X-Apostille-Record": "metadata"} if record else {}


def _evidence_path(run_id: str) -> str:
    if not isinstance(run_id, str) or not _ID.fullmatch(run_id):
        raise ConfigurationError("invalid_run_id")
    return f"/local/v1/runs/{run_id}/evidence"


def _download_evidence(response: Response, directory: str | Path) -> dict[str, Path]:
    """Save byte-preserving exports. Authenticity verification is a separate step."""
    if response.get("receipt_status") != "ready":
        raise APIError("evidence_not_ready", run_id=response.run_id)
    decoded: dict[str, bytes] = {}
    for name in ("manifest", "bundle"):
        encoded = response.get(name + "_base64")
        try:
            if not isinstance(encoded, str) or not encoded or len(encoded) > _MAX_RESPONSE:
                raise ValueError
            value = base64.b64decode(encoded, validate=True)
            if not value or base64.b64encode(value).decode("ascii") != encoded:
                raise ValueError
        except (ValueError, binascii.Error):
            raise APIError("invalid_evidence", run_id=response.run_id) from None
        decoded[name] = value
    target = Path(directory).absolute()
    try:
        target.mkdir(mode=0o700)
    except FileExistsError:
        raise ConfigurationError("download_directory_exists") from None
    except OSError:
        raise ConfigurationError("download_directory_unavailable") from None
    created: list[Path] = []
    try:
        paths = {}
        for name, value in decoded.items():
            path = target / (name + ".json")
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            created.append(path)
            with os.fdopen(fd, "wb") as f:
                # Enforce restrictive bits independently of the caller's umask.
                os.chmod(path, 0o600)
                f.write(value)
                f.flush()
                os.fsync(f.fileno())
            paths[name] = path
        return paths
    except BaseException as exc:
        # Only remove files created by this operation. Never remove a pre-existing
        # directory or recursively delete unrelated files if another process added them.
        for path in created:
            try:
                path.unlink()
            except OSError:
                pass
        try:
            target.rmdir()
        except OSError:
            pass
        if isinstance(exc, OSError):
            raise ConfigurationError("evidence_write_failed") from None
        raise


class _Models:
    def __init__(self, client: Client):
        self._client = client

    def list(self) -> Response:
        return self._client._request("GET", "/v1/models")


class _Evidence:
    def __init__(self, client: Client):
        self._client = client

    def get(self, run_id: str) -> Response:
        return self._client._request("GET", _evidence_path(run_id))

    def download(self, run_id: str, directory: str | Path) -> dict[str, Path]:
        return _download_evidence(self.get(run_id), directory)


class _Chat:
    def __init__(self, client: Client):
        self._client = client

    def create(self, *, model: str, messages: list[dict[str, str]], record: bool = False,
               temperature: float | None = None, top_p: float | None = None,
               max_tokens: int | None = None, response_format: dict | None = None) -> Response:
        payload = _payload(model, messages, stream=False, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format)
        return self._client._request("POST", "/v1/chat/completions", json=payload, headers=_headers(record))

    @contextmanager
    def stream(self, *, model: str, messages: list[dict[str, str]], record: bool = False,
               temperature: float | None = None, top_p: float | None = None,
               max_tokens: int | None = None, response_format: dict | None = None) -> Iterator[Stream]:
        payload = _payload(model, messages, stream=True, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format)
        try:
            with self._client._http.stream("POST", "/v1/chat/completions", json=payload, headers=_headers(record)) as response:
                _check_status(response)
                if response.headers.get("content-type", "").split(";", 1)[0].strip() != "text/event-stream":
                    raise APIError("invalid_stream", run_id=_run_id(response))
                yield Stream(response)
        except httpx.TimeoutException:
            raise APIError("timeout") from None
        except httpx.HTTPError:
            raise APIError("transport_error") from None


class Client:
    """No retries, redirects, or environment proxy settings. TLS by default."""

    def __init__(self, *, base_url: str, token_file: str | Path,
                 allow_insecure: bool = False, timeout: float = 600,
                 ca_file: str | Path | None = None,
                 transport: httpx.BaseTransport | None = None):
        self._http = httpx.Client(base_url=_base_url(base_url, allow_insecure),
                                  headers={"Authorization": "Bearer " + _token(token_file)},
                                  timeout=timeout, follow_redirects=False, trust_env=False,
                                  transport=transport, verify=_tls(ca_file))
        self.models = _Models(self)
        self.chat = _Chat(self)
        self.evidence = _Evidence(self)

    def _request(self, method: str, path: str, **kwargs: Any) -> Response:
        try:
            with self._http.stream(method, path, **kwargs) as response:
                _check_status(response)
                data = bytearray()
                for block in response.iter_bytes(chunk_size=16384):
                    data.extend(block)
                    if len(data) > _MAX_RESPONSE:
                        raise APIError("response_too_large", run_id=_run_id(response))
                return _json(bytes(data), _run_id(response))
        except httpx.TimeoutException:
            raise APIError("timeout") from None
        except httpx.HTTPError:
            raise APIError("transport_error") from None

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()


class _AsyncModels:
    def __init__(self, client: AsyncClient):
        self._client = client

    async def list(self) -> Response:
        return await self._client._request("GET", "/v1/models")


class _AsyncEvidence:
    def __init__(self, client: AsyncClient):
        self._client = client

    async def get(self, run_id: str) -> Response:
        return await self._client._request("GET", _evidence_path(run_id))

    async def download(self, run_id: str, directory: str | Path) -> dict[str, Path]:
        return _download_evidence(await self.get(run_id), directory)


class _AsyncChat:
    def __init__(self, client: AsyncClient):
        self._client = client

    async def create(self, *, model: str, messages: list[dict[str, str]], record: bool = False,
                     temperature: float | None = None, top_p: float | None = None,
                     max_tokens: int | None = None, response_format: dict | None = None) -> Response:
        payload = _payload(model, messages, stream=False, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format)
        return await self._client._request("POST", "/v1/chat/completions", json=payload, headers=_headers(record))

    @asynccontextmanager
    async def stream(self, *, model: str, messages: list[dict[str, str]], record: bool = False,
                     temperature: float | None = None, top_p: float | None = None,
                     max_tokens: int | None = None, response_format: dict | None = None) -> AsyncIterator[AsyncStream]:
        payload = _payload(model, messages, stream=True, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format)
        try:
            async with self._client._http.stream("POST", "/v1/chat/completions", json=payload, headers=_headers(record)) as response:
                _check_status(response)
                if response.headers.get("content-type", "").split(";", 1)[0].strip() != "text/event-stream":
                    raise APIError("invalid_stream", run_id=_run_id(response))
                yield AsyncStream(response)
        except httpx.TimeoutException:
            raise APIError("timeout") from None
        except httpx.HTTPError:
            raise APIError("transport_error") from None


class AsyncClient:
    """Async counterpart; cancellation propagates and closes the response."""

    def __init__(self, *, base_url: str, token_file: str | Path,
                 allow_insecure: bool = False, timeout: float = 600,
                 ca_file: str | Path | None = None,
                 transport: httpx.AsyncBaseTransport | None = None):
        self._http = httpx.AsyncClient(base_url=_base_url(base_url, allow_insecure),
                                       headers={"Authorization": "Bearer " + _token(token_file)},
                                       timeout=timeout, follow_redirects=False, trust_env=False,
                                       transport=transport, verify=_tls(ca_file))
        self.models = _AsyncModels(self)
        self.chat = _AsyncChat(self)
        self.evidence = _AsyncEvidence(self)

    async def _request(self, method: str, path: str, **kwargs: Any) -> Response:
        try:
            async with self._http.stream(method, path, **kwargs) as response:
                _check_status(response)
                data = bytearray()
                async for block in response.aiter_bytes(chunk_size=16384):
                    data.extend(block)
                    if len(data) > _MAX_RESPONSE:
                        raise APIError("response_too_large", run_id=_run_id(response))
                return _json(bytes(data), _run_id(response))
        except httpx.TimeoutException:
            raise APIError("timeout") from None
        except httpx.HTTPError:
            raise APIError("transport_error") from None

    async def aclose(self) -> None:
        await self._http.aclose()

    async def __aenter__(self) -> AsyncClient:
        return self

    async def __aexit__(self, *exc: Any) -> None:
        await self.aclose()

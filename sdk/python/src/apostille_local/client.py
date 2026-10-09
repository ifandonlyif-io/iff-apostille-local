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
_MAX_REQUEST = 1024 * 1024
_MAX_ERROR = 8192
_FUNCTION_NAME = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
_TOOL_ID = re.compile(r"[!-~]{1,128}\Z")
_ERROR_CODES = {
    400: {"query_not_supported", "invalid_json", "unsupported_or_invalid_field",
          "invalid_request", "invalid_record_mode", "tool_calling_unavailable"},
    401: {"unauthorized"}, 403: {"model_forbidden"}, 404: {"not_found"},
    409: {"model_inactive"}, 413: {"request_too_large"}, 415: {"json_required"},
    429: {"capacity_exceeded", "project_capacity_exceeded"},
    502: {"runtime_unavailable", "runtime_rejected", "invalid_runtime_response"},
    503: {"runtime_unavailable", "evidence_unavailable", "draining",
          "evidence_disabled", "id_unavailable"},
    504: {"inference_timeout"},
}


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


def _status_error(response: httpx.Response, data: bytes = b"") -> None:
    if not 200 <= response.status_code < 300:
        codes = {400: "invalid_request", 401: "unauthorized", 403: "forbidden",
                 404: "not_found", 409: "conflict", 413: "request_too_large",
                 415: "unsupported_media_type", 429: "rate_limited",
                 502: "backend_error", 503: "unavailable", 504: "timeout"}
        code = codes.get(response.status_code, "http_error")
        if len(data) <= _MAX_ERROR:
            try:
                error = json.loads(data).get("error", {})
                candidate = error.get("code")
                if isinstance(candidate, str) and candidate in _ERROR_CODES.get(response.status_code, set()):
                    code = candidate
            except (ValueError, UnicodeError, AttributeError, RecursionError):
                pass
        raise APIError(code,
                       status_code=response.status_code, run_id=_run_id(response))


def _read_error(response: httpx.Response) -> bool:
    return (not 200 <= response.status_code < 300 and
            response.headers.get("content-type", "").split(";", 1)[0].strip() == "application/json")


def _check_status(response: httpx.Response) -> None:
    data = bytearray()
    if _read_error(response):
        for block in response.iter_bytes(chunk_size=4096):
            data.extend(block)
            if len(data) > _MAX_ERROR:
                break
    _status_error(response, bytes(data))


async def _acheck_status(response: httpx.Response) -> None:
    data = bytearray()
    if _read_error(response):
        async for block in response.aiter_bytes(chunk_size=4096):
            data.extend(block)
            if len(data) > _MAX_ERROR:
                break
    _status_error(response, bytes(data))


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


def _text_bytes(value: Any, maximum: int, *, empty: bool = False) -> bool:
    if not isinstance(value, str) or (not value and not empty):
        return False
    try:
        return len(value.encode("utf-8")) <= maximum
    except UnicodeError:
        return False


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError
        result[key] = value
    return result


def _json_object(value: Any) -> bool:
    if not _text_bytes(value, 65536):
        return False
    try:
        parsed = json.loads(value, object_pairs_hook=_unique_object)
        json.dumps(parsed, allow_nan=False, ensure_ascii=False).encode("utf-8")
        return isinstance(parsed, dict)
    except (ValueError, UnicodeError, RecursionError):
        return False


def _messages(messages: Any) -> None:
    if not isinstance(messages, list) or not 1 <= len(messages) <= 128:
        raise ConfigurationError("invalid_messages")
    pending: set[str] = set()
    used: set[str] = set()
    for m in messages:
        if not isinstance(m, dict) or "role" not in m:
            raise ConfigurationError("invalid_messages")
        role = m["role"]
        if pending and role != "tool":
            raise ConfigurationError("invalid_messages")
        if role == "tool":
            if (set(m) != {"role", "content", "tool_call_id"} or
                    not isinstance(m["content"], str) or
                    not isinstance(m["tool_call_id"], str) or m["tool_call_id"] not in pending):
                raise ConfigurationError("invalid_messages")
            pending.remove(m["tool_call_id"])
        elif role == "assistant" and "tool_calls" in m:
            calls = m["tool_calls"]
            if (not set(m) <= {"role", "content", "tool_calls"} or
                    (m.get("content") is not None and not isinstance(m["content"], str)) or
                    not isinstance(calls, list) or len(calls) > 16):
                raise ConfigurationError("invalid_messages")
            if not calls:
                if not isinstance(m.get("content"), str) or not m["content"]:
                    raise ConfigurationError("invalid_messages")
                continue
            for call in calls:
                if (not isinstance(call, dict) or set(call) != {"id", "type", "function"} or
                        not isinstance(call["id"], str) or not _TOOL_ID.fullmatch(call["id"]) or
                        call["id"] in used or call["type"] != "function"):
                    raise ConfigurationError("invalid_messages")
                function = call["function"]
                if (not isinstance(function, dict) or set(function) != {"name", "arguments"} or
                        not isinstance(function["name"], str) or not _FUNCTION_NAME.fullmatch(function["name"]) or
                        not _json_object(function["arguments"])):
                    raise ConfigurationError("invalid_messages")
                used.add(call["id"])
                pending.add(call["id"])
        elif (role not in ("system", "user", "assistant") or set(m) != {"role", "content"} or
              not isinstance(m["content"], str) or not m["content"]):
            raise ConfigurationError("invalid_messages")
    if pending:
        raise ConfigurationError("invalid_messages")


def _schema_shape(value: Any, depth: int = 0, count: list[int] | None = None) -> bool:
    """Bound the supported shape; the gateway performs normative schema validation."""
    if count is None:
        count = [0]
    count[0] += 1
    if not isinstance(value, dict) or depth > 12 or count[0] > 256:
        return False
    ordinary = {"type", "required", "enum", "const", "minimum", "maximum", "minLength",
                "maxLength", "minItems", "maxItems", "title", "description"}
    for key, item in value.items():
        if key in ordinary:
            continue
        if key == "additionalProperties":
            if type(item) is not bool:
                return False
        elif key == "properties":
            if not isinstance(item, dict) or not all(_schema_shape(child, depth + 1, count) for child in item.values()):
                return False
        elif key == "items":
            if not _schema_shape(item, depth + 1, count):
                return False
        else:
            return False
    return True


def _tools(tools: Any) -> set[str]:
    if not isinstance(tools, list) or len(tools) > 64:
        raise ConfigurationError("invalid_tools")
    names: set[str] = set()
    for tool in tools:
        if not isinstance(tool, dict) or set(tool) != {"type", "function"} or tool["type"] != "function":
            raise ConfigurationError("invalid_tools")
        f = tool["function"]
        if (not isinstance(f, dict) or not {"name", "parameters"} <= set(f) or
                not set(f) <= {"name", "description", "parameters", "strict"} or
                not isinstance(f["name"], str) or not _FUNCTION_NAME.fullmatch(f["name"]) or
                f["name"] in names or not _schema_shape(f["parameters"]) or
                f["parameters"].get("type") != "object" or
                ("description" in f and not _text_bytes(f["description"], 4096, empty=True)) or
                ("strict" in f and type(f["strict"]) is not bool)):
            raise ConfigurationError("invalid_tools")
        names.add(f["name"])
    return names


def _payload(model: str, messages: list[dict[str, Any]], *, stream: bool,
             temperature: float | None = None, top_p: float | None = None,
             max_tokens: int | None = None, response_format: dict | None = None,
             max_completion_tokens: int | None = None, n: int | None = None,
             stop: str | list[str] | None = None, stream_options: dict | None = None,
             tools: list[dict[str, Any]] | None = None,
             tool_choice: str | dict | None = None,
             parallel_tool_calls: bool | None = None) -> dict[str, Any]:
    if not isinstance(model, str) or not model or not isinstance(messages, list) or not messages:
        raise ConfigurationError("invalid_request")
    _messages(messages)
    payload: dict[str, Any] = {"model": model, "messages": messages, "stream": stream}
    for name, value, upper in (("temperature", temperature, 2), ("top_p", top_p, 1)):
        if value is not None:
            if isinstance(value, bool) or not isinstance(value, (float, int)) or not 0 <= value <= upper or not math.isfinite(value):
                raise ConfigurationError("invalid_sampling_parameter")
            if name == "top_p" and value == 0:
                raise ConfigurationError("invalid_sampling_parameter")
            payload[name] = value
    if max_tokens is not None and max_completion_tokens is not None:
        raise ConfigurationError("conflicting_token_limits")
    for name, value in (("max_tokens", max_tokens), ("max_completion_tokens", max_completion_tokens)):
        if value is not None:
            if type(value) is not int or value < 1:
                raise ConfigurationError("invalid_max_tokens")
            payload[name] = value
    if n is not None:
        if type(n) is not int or n != 1:
            raise ConfigurationError("invalid_n")
        payload["n"] = n
    if stop is not None:
        values = [stop] if isinstance(stop, str) else stop
        if not isinstance(values, list) or not 1 <= len(values) <= 4 or not all(_text_bytes(s, 1024) for s in values):
            raise ConfigurationError("invalid_stop")
        payload["stop"] = stop
    if stream_options is not None:
        if (not stream or not isinstance(stream_options, dict) or
                set(stream_options) != {"include_usage"} or type(stream_options["include_usage"]) is not bool):
            raise ConfigurationError("invalid_stream_options")
        payload["stream_options"] = stream_options
    names = _tools(tools) if tools is not None else set()
    if names:
        payload["tools"] = tools
    if tool_choice is not None:
        if isinstance(tool_choice, str):
            if tool_choice not in ("none", "auto", "required") or (tool_choice != "none" and not names):
                raise ConfigurationError("invalid_tool_choice")
        elif isinstance(tool_choice, dict):
            f = tool_choice.get("function")
            if (set(tool_choice) != {"type", "function"} or tool_choice["type"] != "function" or
                    not isinstance(f, dict) or set(f) != {"name"} or not isinstance(f["name"], str) or f["name"] not in names):
                raise ConfigurationError("invalid_tool_choice")
        else:
            raise ConfigurationError("invalid_tool_choice")
        payload["tool_choice"] = tool_choice
    if parallel_tool_calls is not None:
        if type(parallel_tool_calls) is not bool or not names:
            raise ConfigurationError("invalid_parallel_tool_calls")
        payload["parallel_tool_calls"] = parallel_tool_calls
    if response_format is not None:
        if names and tool_choice != "none":
            raise ConfigurationError("conflicting_response_format")
        if not isinstance(response_format, dict) or set(response_format) != {"type", "json_schema"} or response_format["type"] != "json_schema":
            raise ConfigurationError("invalid_response_format")
        definition = response_format["json_schema"]
        if not isinstance(definition, dict) or set(definition) != {"name", "strict", "schema"} or not isinstance(definition["name"], str) or not definition["name"] or definition["strict"] is not True:
            raise ConfigurationError("invalid_response_format")
        if not _schema_shape(definition["schema"]):
            raise ConfigurationError("invalid_response_format")
        payload["response_format"] = response_format
    try:
        data = json.dumps(payload, allow_nan=False, ensure_ascii=False).encode("utf-8")
    except (TypeError, ValueError, UnicodeError, RecursionError):
        raise ConfigurationError("invalid_request") from None
    if len(data) > _MAX_REQUEST:
        raise ConfigurationError("request_too_large")
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
            if b == 10 and self.cr:
                self.cr = False
                continue
            # Fold event CRLF delimiters, including the LF completing [DONE].
            # After that delimiter, count every trailing byte toward the limit.
            self.cr = b == 13 and not self.done
            self.size += 1
            if self.done:
                # After [DONE] only blank lines may remain until HTTP EOF.
                # Keep the terminal event's reset size as the tail byte count.
                if b not in (10, 13) or self.size > _MAX_EVENT:
                    self.done = False
                    raise APIError("invalid_stream", run_id=self.run_id)
                continue
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
                continue
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
            # Drain through EOF, after the gateway has settled the receipt.
            for block in self._response.iter_bytes():
                yield from parser.feed(block)
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
            # Drain through EOF, after the gateway has settled the receipt.
            async for block in self._response.aiter_bytes():
                for result in parser.feed(block):
                    yield result
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


class _Capabilities:
    def __init__(self, client: Client):
        self._client = client

    def get(self) -> Response:
        return self._client._request("GET", "/local/v1/capabilities")


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

    def create(self, *, model: str, messages: list[dict[str, Any]], record: bool = False,
               temperature: float | None = None, top_p: float | None = None,
               max_tokens: int | None = None, response_format: dict | None = None,
               max_completion_tokens: int | None = None, n: int | None = None,
               stop: str | list[str] | None = None, stream_options: dict | None = None,
               tools: list[dict[str, Any]] | None = None,
               tool_choice: str | dict | None = None,
               parallel_tool_calls: bool | None = None) -> Response:
        payload = _payload(model, messages, stream=False, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format,
                           max_completion_tokens=max_completion_tokens, n=n, stop=stop,
                           stream_options=stream_options, tools=tools, tool_choice=tool_choice,
                           parallel_tool_calls=parallel_tool_calls)
        return self._client._request("POST", "/v1/chat/completions", json=payload, headers=_headers(record))

    @contextmanager
    def stream(self, *, model: str, messages: list[dict[str, Any]], record: bool = False,
               temperature: float | None = None, top_p: float | None = None,
               max_tokens: int | None = None, response_format: dict | None = None,
               max_completion_tokens: int | None = None, n: int | None = None,
               stop: str | list[str] | None = None, stream_options: dict | None = None,
               tools: list[dict[str, Any]] | None = None,
               tool_choice: str | dict | None = None,
               parallel_tool_calls: bool | None = None) -> Iterator[Stream]:
        payload = _payload(model, messages, stream=True, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format,
                           max_completion_tokens=max_completion_tokens, n=n, stop=stop,
                           stream_options=stream_options, tools=tools, tool_choice=tool_choice,
                           parallel_tool_calls=parallel_tool_calls)
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
        self.capabilities = _Capabilities(self)
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


class _AsyncCapabilities:
    def __init__(self, client: AsyncClient):
        self._client = client

    async def get(self) -> Response:
        return await self._client._request("GET", "/local/v1/capabilities")


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

    async def create(self, *, model: str, messages: list[dict[str, Any]], record: bool = False,
                     temperature: float | None = None, top_p: float | None = None,
                     max_tokens: int | None = None, response_format: dict | None = None,
               max_completion_tokens: int | None = None, n: int | None = None,
               stop: str | list[str] | None = None, stream_options: dict | None = None,
               tools: list[dict[str, Any]] | None = None,
               tool_choice: str | dict | None = None,
               parallel_tool_calls: bool | None = None) -> Response:
        payload = _payload(model, messages, stream=False, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format,
                           max_completion_tokens=max_completion_tokens, n=n, stop=stop,
                           stream_options=stream_options, tools=tools, tool_choice=tool_choice,
                           parallel_tool_calls=parallel_tool_calls)
        return await self._client._request("POST", "/v1/chat/completions", json=payload, headers=_headers(record))

    @asynccontextmanager
    async def stream(self, *, model: str, messages: list[dict[str, Any]], record: bool = False,
                     temperature: float | None = None, top_p: float | None = None,
                     max_tokens: int | None = None, response_format: dict | None = None,
               max_completion_tokens: int | None = None, n: int | None = None,
               stop: str | list[str] | None = None, stream_options: dict | None = None,
               tools: list[dict[str, Any]] | None = None,
               tool_choice: str | dict | None = None,
               parallel_tool_calls: bool | None = None) -> AsyncIterator[AsyncStream]:
        payload = _payload(model, messages, stream=True, temperature=temperature,
                           top_p=top_p, max_tokens=max_tokens, response_format=response_format,
                           max_completion_tokens=max_completion_tokens, n=n, stop=stop,
                           stream_options=stream_options, tools=tools, tool_choice=tool_choice,
                           parallel_tool_calls=parallel_tool_calls)
        try:
            async with self._client._http.stream("POST", "/v1/chat/completions", json=payload, headers=_headers(record)) as response:
                await _acheck_status(response)
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
        self.capabilities = _AsyncCapabilities(self)
        self.chat = _AsyncChat(self)
        self.evidence = _AsyncEvidence(self)

    async def _request(self, method: str, path: str, **kwargs: Any) -> Response:
        try:
            async with self._http.stream(method, path, **kwargs) as response:
                await _acheck_status(response)
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

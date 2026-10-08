# Apostille Local Python SDK

Python 3.11+; software preview. Install from a reviewed checkout:

```sh
python -m pip install ./sdk/python
```

The distribution name is `apostille-local`; the import is `apostille_local`.
There is no claim that this preview has been published to PyPI.

For an offline installation, first verify the approved preview bundle's
`SHA256SUMS` through your artifact-transfer process, then install its locked
dependencies and SDK wheel into a virtual environment:

```sh
PREVIEW=./dist/0.1.0-alpha.1
python -m pip install --no-index --find-links "$PREVIEW/wheelhouse" \
  --require-hashes -r "$PREVIEW/sdk/python/requirements-linux-py311.lock"
python -m pip install --no-index --no-deps \
  "$PREVIEW/python/apostille_local-0.1.0a1-py3-none-any.whl"
```

The dependency lock targets Linux x86_64 / Python 3.11; the bundled dependency
wheels are pure Python. Validate other interpreter/platform combinations before
claiming support. Installation does not download a model or inference runtime.

## Synchronous client

Create a project token with the administrator's CLI, save the raw token in a
restricted file (for example mode `0600`), and keep it out of source control.
The SDK reads the file at client construction; recreate the client after rotation.
`base_url` is the gateway origin, without `/v1`.

```python
from apostille_local import Client

with Client(base_url="https://gateway.example.internal:8443",
            ca_file="/etc/apostille/customer-ca.pem",
            token_file="/run/secrets/apostille-project-token") as client:
    models = client.models.list()
    capabilities = client.capabilities.get()
    if not models["data"]:
        raise RuntimeError("No authorized active model is ready")
    model = models["data"][0]["id"]
    result = client.chat.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with a short greeting."}],
        max_tokens=64,
        record=True,
    )
    # Consume the answer inside the approved application; avoid sensitive logs.
    answer = result["choices"][0]["message"]["content"]
    run_id = result.run_id
    if run_id:
        evidence = client.evidence.get(run_id)
        # pending, ready, or failed; no automatic polling or retry.
        status = evidence["receipt_status"]
```

`record=True` sends `X-Apostille-Record: metadata`. It requests a signed metadata
assertion, not retention of the prompt/completion. A receipt does not establish
output integrity, actual model execution, truth, or non-exfiltration. Evidence is
authorized by the same project token. An embedded signing key is not a trust pin.

## Download and independently verify evidence

Once the receipt is ready, download the exact signed artifacts into a **new**
directory whose parent already exists:

```python
with Client(base_url="https://gateway.example.internal:8443",
            ca_file="/etc/apostille/customer-ca.pem",
            token_file="/run/secrets/apostille-project-token") as client:
    result = client.chat.create(
        model="approved-model",
        messages=[{"role": "user", "content": "Reply with a short greeting."}],
        record=True,
    )
    paths = client.evidence.download(result.run_id, "./run-export")
    # paths["manifest"] and paths["bundle"] are pathlib.Path objects.
```

`download` reads the ready response's `manifest_base64` and `bundle_base64`,
decodes them strictly, and writes their exact bytes as `manifest.json` and
`bundle.json`. Do not serialize the `manifest` or `bundle` JSON objects yourself:
changing whitespace or key order can change the artifact bytes used by verification.
The new directory uses mode `0700` and files use `0600` on POSIX systems; configure
equivalent access controls separately on other systems. Existing directories are
rejected and write failures clean up the files this call created.

Pending or failed receipts raise `APIError` with code `evidence_not_ready`;
missing or malformed encoded artifacts raise `invalid_evidence`. There is no
automatic polling, retry, or fallback to reserializing JSON objects. Download
before the gateway's 24-hour retention expires; exported copies have their own
local retention policy.

Verify the downloaded artifact with the independent local verifier:

```sh
./bin/apostille-local-verify \
  --artifact ./run-export/manifest.json \
  --bundle ./run-export/bundle.json \
  --producer-pin "$EXPECTED_PRODUCER_PIN"
```

Provision `EXPECTED_PRODUCER_PIN` independently through the receiver's trusted
channel; do not derive a trust pin from the API response. Downloading does not
verify a signature. Successful verification establishes the signed metadata
assertion, not prompt/output content or actual model execution.

## Streaming and cancellation

```python
with Client(base_url="https://gateway.example.internal:8443",
            token_file="/run/secrets/apostille-project-token") as client:
    with client.chat.stream(model="approved-model",
                            messages=[{"role": "user", "content": "Hello"}],
                            stream_options={"include_usage": True}) as stream:
        run_id = stream.run_id
        for chunk in stream:
            if not chunk["choices"]:
                usage = chunk.get("usage")  # Final usage chunk has no choices.
                continue
            delta = chunk["choices"][0].get("delta", {})
            # Deliver delta to your authorized UI; do not log it.
            if user_cancelled():  # application-provided cancellation signal
                break
```

The stream must remain inside its context manager. Leaving the context closes the
HTTP response, including on early break or error. The gateway can observe that
disconnect and cancel backend work. A `[DONE]` terminator is required for complete
streams; silent EOF is an `incomplete_stream` error. Each SSE event is capped at
1 MiB; ordinary JSON responses are capped at 4 MiB. These SDK limits can be stricter
than a future server; this version intentionally fails closed.

Structured-output deltas are provisional: the gateway validates the accumulated
output against the schema before sending `[DONE]`. Do not execute downstream
actions using partial JSON or treat a finish-reason chunk alone as success.
If validation fails, the stream raises an error even if some text was delivered.
The same rule applies to streamed tool-call arguments. Assemble fragments by tool
index in memory and wait for a complete stream before asking the customer's tool
executor to act. An interrupted stream may have no final usage event.

## Customer-owned tools and frameworks

Customers install and configure their own Agent framework and tools. The gateway
and this SDK transport function-call messages; neither executes functions, mounts
customer files, installs a framework, or contacts tool endpoints. Enable a tested
tool-call parser for the active model through the administrator's configuration
before requesting tool calls. Read `client.capabilities.get()` for the current
project's interface capabilities; capability flags are not hardware or framework
certification.

Here is a synthetic, synchronous two-request exchange. The application recognizes
one allowlisted function and supplies a synthetic result itself:

```python
import json
from apostille_local import Client

tools = [{"type": "function", "function": {
    "name": "lookup_synthetic", "strict": True,
    "parameters": {"type": "object", "properties": {"id": {"type": "string"}},
                   "required": ["id"], "additionalProperties": False},
}}]
messages = [{"role": "user", "content": "Look up synthetic record demo-1."}]

with Client(base_url="https://gateway.example.internal:8443",
            ca_file="/etc/apostille/customer-ca.pem",
            token_file="/run/secrets/apostille-project-token") as client:
    result = client.chat.create(
        model="approved-model", messages=messages, tools=tools,
        tool_choice="required", parallel_tool_calls=False,
    )
    message = result["choices"][0]["message"]
    if result["choices"][0]["finish_reason"] != "tool_calls":
        raise RuntimeError("No complete tool call was returned")
    calls = message["tool_calls"]
    messages.append({"role": "assistant", "content": message.get("content"),
                     "tool_calls": calls})
    for call in calls:
        if call["function"]["name"] != "lookup_synthetic":
            raise RuntimeError("Unapproved tool")
        arguments = json.loads(call["function"]["arguments"])
        # Replace this synthetic lookup with your authorized, bounded tool executor.
        value = "synthetic-match" if arguments["id"] == "demo-1" else "not-found"
        messages.append({"role": "tool", "tool_call_id": call["id"], "content": value})
    final = client.chat.create(model="approved-model", messages=messages,
                               tools=tools, tool_choice="none")
```

Applications must still enforce the tool's authorization, arguments, timeouts and
side-effect policy. Never treat a generated function name or arguments as a shell
command. A metadata receipt with `finish_reason: tool_calls` records generation
completion; it does not prove that the customer's tool ran.

External OpenAI-compatible clients use `https://gateway.example.internal:8443/v1`
as their API base URL. This SDK uses the origin **without `/v1`**, since it also
accesses `/local/v1` routes. Configure external clients with the project token,
customer CA bundle, no automatic retries and no cloud fallback. See the repository
[interoperability guide](../../docs/INTEGRATIONS.md) for the supported wire
contract. Installing an external client or framework is the customer's choice;
it is not an SDK dependency or a claim of tested framework support.
Build history messages from the supported fields shown above. Do not submit an
entire external response object's `model_dump()` unchanged: fields such as
`refusal` and legacy `function_call`, even when `null`, are outside this contract.

## Asynchronous client

```python
from apostille_local import AsyncClient

async def consume():
    async with AsyncClient(base_url="https://gateway.example.internal:8443",
                           token_file="/run/secrets/apostille-project-token") as client:
        async with client.chat.stream(
            model="approved-model",
            messages=[{"role": "user", "content": "Hello"}],
        ) as stream:
            async for chunk in stream:
                await display_authorized_chunk(chunk)  # application function
```

All async calls mirror synchronous calls (`await client.models.list()`,
`await client.capabilities.get()`,
`await client.chat.create(...)`, `await client.evidence.get(run_id)`,
`await client.evidence.download(run_id, "./run-export")`). Task
cancellation propagates; the context manager closes the response.

## Supported request surface

- Text `system`, `user`, and plain `assistant` messages have nonempty string content.
  Assistant messages with function `tool_calls` can omit content or use `null`;
  `tool` replies require string content and the matching `tool_call_id`. Answer
  every pending call once before the next ordinary message. At most 128 messages
  and a 1 MiB request body; the SDK checks these bounds before sending.
- `model`, `messages`, `temperature`, `top_p`, `max_tokens` and `response_format`.
  `max_completion_tokens` is an alternative to `max_tokens`; do not send both.
- One choice: omit `n` or set `n=1`. Other values are rejected.
- `stop` is a nonempty string or 1–4 nonempty strings, each at most 1024 UTF-8 bytes.
- Streaming accepts `stream_options={"include_usage": True}` (or `False`).
  This option is rejected for ordinary `chat.create` requests.
- `tools` declares up to 64 named functions with object parameter schemas;
  `tool_choice` accepts `none`, `auto`, `required`, or a named function selector.
  `parallel_tool_calls` is a boolean with tools present. At most 16 calls per
  assistant message; each argument string must contain a JSON object and be at
  most 64 KiB. Tools can use the bounded schema subset below. An empty `tools=[]`
  is omitted. Tool declarations cannot be combined with `response_format` unless
  `tool_choice="none"` explicitly disables generating tool calls.
- `response_format` uses `{type: "json_schema", json_schema: {name, strict: true,
  schema: {type: "object", ...}}}`. The schema is a JSON object; its declared root
  type may be an object, array or scalar. This is a bounded JSON Schema subset, not
  complete JSON Schema support; see [API specification](../../api/openapi.yaml).
- No image/audio inputs, server-side tool execution, RAG, arbitrary sampling
  extensions, automatic retry, automatic evidence polling, or cloud fallback.

HTTPS certificate verification is on. Set `ca_file="/path/customer-ca.pem"` for
the customer's private CA trust bundle; environment CA/proxy settings are not
used. Both synchronous and asynchronous clients support this option. HTTP is rejected unless
`allow_insecure=True` **and** the URL names loopback (`localhost`, `127.0.0.1`,
`::1`). This exception is for local development. Redirects and environment proxy
settings are disabled. The default timeout is 600 seconds; set `timeout=` to your
client budget. A timeout does not make a request safe to retry.

Errors are `APIError` with a fixed sanitized `code`, optional `status_code` and
`run_id`. Known gateway error codes such as `model_inactive`, `model_forbidden`,
`capacity_exceeded`, `project_capacity_exceeded` and `draining` are preserved;
unknown codes use a generic status-based fallback. Error messages and backend
body contents are never included. `ConfigurationError` reports
local validation failures. The SDK never logs; application logging and HTTP debug
instrumentation remain the caller's responsibility. Do not expose tokens through
custom transports, instrumentation or object inspection.

## Tests and build

```sh
python -m unittest discover -s sdk/python/tests -v
python -m build sdk/python
```

Tests use synthetic data and `httpx.MockTransport`; they do not verify a GPU,
container, deployed gateway or actual model output. Production applications
should not replace the default transport with a retrying or unverified transport.

## Optional workflow recorder

`apostille_local.workflow.WorkflowRecorder` records a bounded set of workflow
assertions through the local `apostille-workflow` Go CLI. It uses explicit key
and policy file paths, a private per-agent archive and independent producer pins.
It adds no Python cryptography or FL runtime dependency. Training results and
receipt outcomes remain separate. See the repository's
[workflow guide](../../docs/WORKFLOW_EVIDENCE.md) for the schema, examples,
retention policy and offline verification contract.

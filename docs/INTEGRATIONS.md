# Customer-owned framework integrations

Customers install their chosen agent framework, UI, knowledge base and tool
sandbox. Apostille Local supplies an authenticated inference boundary. It does
not install those frameworks, execute their tools or take over their state.

```text
Customer application / framework / tool sandbox
  | project token + customer TLS trust
  v
Apostille Local gateway --> pinned local vLLM runtime --> approved model
  |
  +--> optional signed run metadata (no prompt, answer or tool arguments)
```

## Connection contract

The administrator supplies these settings through an approved local channel:

| Setting | Value |
| --- | --- |
| Chat Completions base URL | `https://gateway.example.internal:8443/v1` |
| Apostille Local SDK base URL | `https://gateway.example.internal:8443` (without `/v1`) |
| Authentication | Project bearer token from a restricted local file |
| TLS trust | Customer CA bundle; verify certificates, disable redirects |
| Discovery | `GET /v1/models` and `GET /local/v1/capabilities` with the same token |
| Retry policy | Disable automatic request retries, including framework wrappers |
| Model | Choose an ID returned for this project; clients cannot install or activate models |

Do not point clients directly at the runtime, use an OpenAI service key or enable
a cloud fallback. Disable framework tracing, chat history, external telemetry and
environment proxy inheritance unless separately reviewed for the customer's
environment. Client callbacks and exception logging can retain content even when
the gateway does not. Enforce network policy at the host as described in
[deployment instructions](../deploy/README.md).

### Discover before enabling features

`GET /local/v1/capabilities` returns contract version `"1"`:

```json
{
  "object": "apostille_local.capabilities",
  "contract_version": "1",
  "api_family": "chat_completions",
  "gateway_version": "0.1.0-alpha.2",
  "models": [{
    "id": "qwen3-4b",
    "max_context": 4096,
    "max_output_tokens": 512,
    "max_concurrent_requests": 1,
    "features": {
      "text": true,
      "streaming": true,
      "stream_usage": true,
      "json_schema": true,
      "tool_calling": false
    }
  }],
  "evidence": {"metadata": false, "retention_seconds": 86400},
  "compatible_apis": [{"family": "anthropic_messages", "path": "/v1/messages", "anthropic_version": "2023-06-01"}],
  "tool_execution": "client",
  "unsupported": ["responses", "embeddings", "multimodal", "server_tool_execution"]
}
```

Limits are configured admission limits, not measured performance guarantees.
`models` contains only the healthy active model authorized for this project;
otherwise it is empty. Discovery does not reserve capacity and readiness can
change before inference. Check the contract version and fail clearly on a
required unsupported feature. Runtime readiness does not establish hardware or
framework qualification.

## Supported Chat Completions subset

The exact field contract is in [OpenAPI](../api/openapi.yaml).

| Feature | Contract |
| --- | --- |
| Messages | Text `system`, `user`, `assistant`; assistant function calls and matching `tool` results |
| Sampling | `temperature`, `top_p`, `stop` (string or up to four strings) |
| Output limit | `max_tokens` or `max_completion_tokens`; never both |
| Choices | Omit `n` or send `n: 1` |
| Streaming | `stream: true`; optional `stream_options: {"include_usage": true}` |
| Structured output | `response_format.type: json_schema`, `strict: true`, bounded schema subset |
| Function calling | Function `tools`, `tool_choice` (`none`, `auto`, `required`, named function), `parallel_tool_calls` |
| Receipts | Optional `X-Apostille-Record: metadata`; separate project-scoped evidence download |

Unknown fields are rejected. Responses API, embeddings, images, audio, legacy
`functions`/`function_call`, arbitrary sampling extensions and server-executed
tools are not supported. Do not describe the endpoint as fully OpenAI compatible.
Use Chat Completions mode in frameworks that also support the Responses API.
Omit unused optional fields instead of sending `null`; nullable assistant
content alongside tool calls is the explicit exception.

Tools must be enabled for the active model by an administrator. The preview
accepts the pinned model setting `tool_call_parser: "hermes"` together with
`runtime_profile: "vllm-chat-v1"`; a parser without this profile is rejected at
configuration validation. Omitting the parser disables tool calling. Both
settings are included in asset metadata and the runtime launch
configuration. Qualify the exact image, model revision and chat template before
offering that feature to users. This setting does not install an agent framework.

Administrators select the pinned `runtime_profile: "vllm-chat-v1"` when preparing
tool-capable vLLM assets. The gateway then translates a validated named call's
upstream `stop` finish reason to `tool_calls`. Clients use the same contract
across AMD and NVIDIA deployments. See [runtime profiles and API acceptance](RUNTIME_COMPATIBILITY.md).

Function parameters use the same bounded JSON Schema subset as structured output.
The gateway checks the function name and completed JSON argument object. Set a
function's `strict: true` to also enforce its declared parameter schema. It does
not establish that a tool request is safe or
authorized in the customer's business system. Validate permissions and enforce
an explicit local tool allowlist before execution; never turn model output into
a shell command or unrestricted URL fetch.

Keep assistant tool-call IDs and match each result using `tool_call_id`. Send
tool definitions again when asking for further tool calls; completed tool history
can also be followed by a text-only request without definitions. Do not run
tools from partial streamed arguments. Accumulate and validate the complete
message first; a `tool_calls` finish reason means the generation requested tools,
not that those tools have run successfully.

When combining tool history with structured text output, set `tool_choice:
"none"`. Simultaneous structured text output and a non-none tool choice are
rejected, so the final response contract is unambiguous.

### Streaming and failure handling

When usage is requested, the final usage chunk has `choices: []`. Do not index
`choices[0]` without checking whether the array is empty. Token counts describe
the runtime's report. Close the response on cancellation; disable retries for
both ordinary and streamed inference requests.

Treat stream data as provisional until `[DONE]`. A finish-reason chunk alone is
not a success signal; the gateway can still reject final JSON/tool arguments.
Some third-party SDKs hide the SSE terminator and treat EOF as completion. The
native Apostille Local SDK checks `[DONE]` explicitly; adapters for other SDKs
must preserve that check before committing consequential actions.

Errors use an `error` object with stable `code`, sanitized `message` and `type`.
No backend error text is returned. Distinguish authentication/authorization,
inactive models, capacity limits, runtime failure and timeout. An HTTP failure
or interrupted stream does not establish that no generation took place. Receipt
status remains separate from the inference result. Stream receipts are signed
after the final output is written and flushed successfully; an immediate
evidence query can still return `pending` while signing completes. See the
[SDK evidence guide](../sdk/python/README.md).

## Runnable official Python client example

The [example](../examples/openai_client.py) uses `openai==2.29.0` and
`httpx==0.28.1`. These are example/test dependencies, not additions to the
Apostille Local SDK. Prepare and review their dependencies before offline
deployment; the project's SDK wheelhouse does not bundle the OpenAI client.

```sh
python -m pip install openai==2.29.0 httpx==0.28.1
python examples/openai_client.py \
  --base-url https://gateway.example.internal:8443 \
  --ca-file /etc/apostille/customer-ca.pem \
  --token-file /run/secrets/apostille-project-token
```

The example discovers capabilities, runs a synthetic greeting and prints only a
completion status. Add `--tool-demo` for a synthetic local lookup roundtrip after
the administrator has enabled and qualified tool calling. It neither prints nor
persists prompts, answers, tool arguments or keys. Real tool execution belongs
in the customer's authorization and sandbox boundary.

## Anthropic Messages API subset

`POST /v1/messages` lets an application written for the official Anthropic SDK
use the same validated, locally served inference as Chat Completions. The gateway
translates the request into its internal Chat Completions request, runs it through
the same admission, validation, model checks, receipts and runtime response
validation, and translates the validated result back. **Requests never go to
Anthropic.** The model is the administrator's local model, not Claude. Do not put a
real Anthropic API key in the client, and do not describe this endpoint as Claude
or as fully Anthropic compatible. The exact field contract is in
[OpenAPI](../api/openapi.yaml).

### Connection settings

| Setting | Value |
| --- | --- |
| Base URL | `https://gateway.example.internal:8443` **without** `/v1` (the SDK appends `/v1/messages`) |
| Authentication | The project token as `api_key` (`X-Api-Key`) **or** as `auth_token` (`Authorization: Bearer`). Exactly one; sending both is rejected with 401 |
| Version header | `anthropic-version: 2023-06-01` (the SDK default); any other value is rejected. Any `anthropic-beta` header is rejected |
| Retries | `max_retries=0` |
| TLS trust | Customer CA through an explicit HTTP client, `trust_env=False`, `follow_redirects=False` |
| Environment | Pass credentials explicitly; remove `ANTHROPIC_LOG` and `ANTHROPIC_CUSTOM_HEADERS` before importing the SDK; no environment proxies |

Discovery stays on `GET /local/v1/capabilities` (Bearer), which lists the family
under `compatible_apis`. `GET /v1/models` keeps its OpenAI-style shape.

### Supported subset

| Feature | Contract |
| --- | --- |
| Request fields | `model`, `max_tokens` (required, within the model limit), `messages`, `system`, `stream`, `temperature` (0..1), `top_p`, `tools`, `tool_choice`, `output_config` |
| `system` | A non-empty string, or one text block |
| Messages | `user`/`assistant` with string content or blocks; at most one text block per message; the last message must be `user` |
| User blocks | `text`; `tool_result` with string or one-text-block `content` (all results before the text block) |
| Assistant blocks | `text` (before any tool use) and `tool_use` |
| Tools | Custom tools: `name`, `description`, `input_schema` (same bounded schema subset as Chat Completions), `strict`, optional `type: "custom"` |
| `tool_choice` | `auto`, `any`, `none`, `tool` (named), with optional `disable_parallel_tool_use` (not with `none`) |
| Structured output | `output_config.format` of type `json_schema` only; not combinable with a non-`none` tool choice |
| Streaming | `stream: true`; usage is always requested internally |
| Receipts | Optional `X-Apostille-Record: metadata`, as on Chat Completions |

Current SDK releases (for example 1.8.0) expose no `temperature`/`top_p`
arguments; send them with `extra_body` if needed.

### Mapping

| Gateway/runtime | Messages API |
| --- | --- |
| `stop` / `length` / `tool_calls` | `end_turn` / `max_tokens` / `tool_use` (`stop_sequence` is always `null`) |
| `prompt_tokens` / `completion_tokens` | `usage.input_tokens` / `usage.output_tokens`; runtime usage is required |
| Message id | `msg_` followed by the run ID (`X-Apostille-Run-ID`) |
| Text content | One `text` block when non-empty; `content` may be `[]` |
| Assistant `tool_use` (`id`, `name`, `input`) | Internal tool call; `input` is passed through as compact JSON |
| User `tool_result` | One internal `tool` message per result, then the text block as a `user` message |
| Generated tool call | `tool_use` block with the validated arguments object as `input` |

The same tool rules as Chat Completions apply: IDs must pair, tool input must be a
JSON object, strict definitions enforce their schema, and the customer
application must authorize every action. Tools are never executed by the gateway.

### Streaming commit rule

Text deltas are forwarded as they pass validation, but tool calls are not: their
arguments stay in a bounded buffer and `tool_use` blocks (`content_block_start`,
one `input_json_delta` carrying the complete arguments, `content_block_stop`)
are emitted only after the whole runtime stream, finish reason, usage and
structured/tool arguments validated. A response is final only when
`message_delta` carries a non-null `stop_reason` **and** `message_stop` arrives.
Runtime validation failures produce an `event: error` frame (`api_error`,
`invalid_runtime_stream`) and never a `message_delta` or `message_stop`. The
SDK's `stream.get_final_message()` can return a partial snapshot if the
connection ends early without an error event, so check `stop_reason is not None`
before acting. Never run a tool from a partial stream; text already shown to a
user is provisional until the final events arrive. Disable retries and close the
response on cancellation. A detected write or flush failure leaves a failed
receipt; an error frame may not reach a disconnected client. Receipt signing
starts after the final stream output succeeds, so query evidence separately
and allow for `pending` while signing completes.

### Errors

Errors use `{"type":"error","error":{"type":<Anthropic type>,"message":<code>}}`
with the same status codes as Chat Completions. `message` is a stable gateway
code such as `unsupported_or_invalid_field`, `model_forbidden`,
`anthropic_version_unsupported` or `beta_not_supported`; no backend text is
returned. Types: 400/409/415 `invalid_request_error`, 401 `authentication_error`,
403 `permission_error`, 404 `not_found_error`, 413 `request_too_large`, 429
`rate_limit_error`, 5xx `api_error`. Global checks (a query string is rejected
with `query_not_supported`) use the gateway's own error shape.

Receipts are unchanged: the manifest records the internal finish reasons
(`stop`, `length`, `tool_calls`), not the Messages names, and contains no prompt,
answer or tool arguments.

### Not supported

Claude Code and the Claude Agent SDK as clients, `/v1/messages/count_tokens`,
an Anthropic-shaped `/v1/models`, the beta namespace (`?beta=true`,
`anthropic-beta`), batches, files, images and documents, thinking,
prompt caching (`cache_control`), server tools and MCP, `stop_sequences`,
`top_k`, `metadata`, `citations`, `tool_result` with `is_error: true` (put error
details in the result content instead), assistant prefill (a final `assistant`
message), and `eager_input_streaming`. These are rejected, not ignored.

### Runnable Anthropic client example

The [example](../examples/anthropic_client.py) uses `anthropic==1.8.0`, which runs
on `httpx2`. These are example/test dependencies only, not additions to the
Apostille Local SDK; prepare them like any other customer dependency.

```sh
python -m pip install anthropic==1.8.0
python examples/anthropic_client.py \
  --base-url https://gateway.example.internal:8443 \
  --ca-file /etc/apostille/customer-ca.pem \
  --token-file /run/secrets/apostille-project-token
```

It discovers capabilities, runs a synthetic greeting and prints only a
completion status; `--tool-demo` adds a synthetic local lookup round trip. The
[executable test](../tests/anthropic_live.py) exercises the SDK's `create`,
`stream`, tools, structured output, typed errors, cancellation and async client
against a synthetic runtime.

## Framework adoption checklist

The TLS integration suite also runs actual `langchain-openai==1.1.11` with
`langchain-core==1.2.18`, `openai==2.29.0` and `httpx==0.28.1`. The
[executable test](../tests/langchain_live.py) shows `ChatOpenAI` with explicit
sync/async HTTP transports, private CA, `trust_env=False`, disabled retries,
stream usage, schema output and named/required tools. These are development
dependencies in [the framework test requirements](../tests/requirements-framework.txt),
not part of the SDK's runtime dependency list or offline wheelhouse. Tests use
a synthetic runtime; another framework/version still needs the checks below.

1. Pin the framework and its provider adapter; configure the exact gateway URL,
   project token, CA, timeout and disabled retries.
2. Capture its synthetic requests in a test environment and compare them with
   this contract. Disable unsupported fields instead of silently dropping them.
3. Verify text, streaming usage, cancellation, tool roundtrips, malformed output,
   forbidden model access and capacity errors against the intended deployment.
4. Check framework logs, history databases, traces, caches and backups for
   synthetic input/output markers. Enforce tool and network policies separately.
5. Record exact framework, model, image, driver and GPU versions in the customer's
   acceptance report. A working OpenAI-style adapter is not certification of
   GAIA, NeMo Agent Toolkit, OpenShell or a complete agent workflow.

Protocol references: [Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
and the [pinned official Python SDK](https://github.com/openai/openai-python/tree/v2.29.0).

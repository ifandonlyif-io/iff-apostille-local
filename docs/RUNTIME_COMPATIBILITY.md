# Runtime compatibility and API acceptance

## Current boundary

The client interface is shared by AMD and NVIDIA deployments. The administrator
selects a vendor-specific vLLM image and a model bundle; clients connect to the
gateway using the same TLS, authorization and Chat Completions contract.

| Layer | Implemented check | What remains unverified |
| --- | --- | --- |
| Clients | Native SDK, OpenAI Python 2.29.0 and LangChain OpenAI 1.1.11 against a real TLS gateway and synthetic runtime | Other client versions and complete customer agent workflows |
| Runtime protocol | Explicit vLLM wire profile, malformed-response and cancellation regressions | Actual image/model behavior on a GPU |
| NVIDIA deployment | Compose render, one selected GPU, private runtime and gateway isolation | CUDA image startup, driver compatibility and real inference |
| AMD deployment | Compose render, selected render node plus KFD, render/video groups and private runtime | ROCm image startup, driver compatibility and real inference |
| Installed service | Synthetic API checker described below | GPU identity, offline isolation, performance, recovery and evidence qualification |

There is currently no tested GPU tuple. Do not infer support for a card, model or
quantization from these software checks. Record actual acceptance in
[SUPPORT_MATRIX.md](SUPPORT_MATRIX.md).

## Upstream work used as reference

These projects inform implementation and future backend selection. Their test
results are evidence for their tested configurations, not for this gateway.

| Project | Evidence examined | Use here |
| --- | --- | --- |
| [vLLM](https://github.com/vllm-project/vllm) | [GPU CI infrastructure](https://github.com/vllm-project/ci-infra) includes AMD and NVIDIA runners; [official installation matrix](https://docs.vllm.ai/en/stable/getting_started/installation/gpu/) distinguishes hardware/build requirements | Initial runtime, with separate vendor images and local acceptance still required |
| [llama.cpp](https://github.com/ggml-org/llama.cpp) | [Successful CUDA/ROCm CI run](https://github.com/ggml-org/llama.cpp/actions/runs/37784768756) at commit `1167d3f42c2b4dc5469bf4e696cb8c678ca79110`; the pinned [workflow](https://github.com/ggml-org/llama.cpp/blob/1167d3f42c2b4dc5469bf4e696cb8c678ca79110/.github/workflows/ci-self-hosted-cuda.yml) tests backend/model behavior | Candidate for a later GGUF/backend adapter; no llama.cpp launcher or asset support in this preview |
| [GPUStack](https://github.com/gpustack/gpustack) | Model lifecycle, catalog and multi-backend architecture | Operational reference; its orchestration privileges are not part of the gateway deployment |
| [LocalAI](https://github.com/mudler/LocalAI) | [GPU verification table](https://localai.io/docs/features/gpu-acceleration/#verified) identifies the tested backend as well as hardware | Follow the same per-backend evidence discipline; do not generalize llama.cpp results to vLLM |

GAIA, NeMo Agent Toolkit and customer tool sandboxes can be evaluated as clients
under [INTEGRATIONS.md](INTEGRATIONS.md). No such framework is installed into the
gateway. A compatible API connection does not validate tool permissions, agent
behavior or third-party logging.

## Explicit vLLM wire profile

Prepare assets with:

```sh
apostille-local-admin assets prepare \
  --source "$MODEL_SNAPSHOT" --image-archive "$RUNTIME_ARCHIVE" --out "$NEW_BUNDLE" \
  --id "$MODEL_ID" --revision "$MODEL_REVISION" --license "$MODEL_LICENSE" \
  --image "$RUNTIME_IMAGE" --runtime-profile vllm-chat-v1 \
  --precision bfloat16 --max-context 4096 --max-tokens 512 --max-concurrent 1
```

For an approved tool-capable model, also pass `--tool-call-parser hermes`. The
profile does not enable tool calling by itself. Both settings are part of the
hashed model manifest. Copy its fields into the catalog, import the new bundle
and activate it through the [administrator workflow](../deploy/README.md).
Editing the catalog alone causes manifest validation to fail.

The [vLLM 0.15.0 Chat Completions implementation](https://github.com/vllm-project/vllm/blob/v0.15.0/vllm/entrypoints/openai/chat_completion/serving.py)
can end a **named** function call with `finish_reason: "stop"`. The public gateway
contract requires `tool_calls` for a completed function call. The explicit
`vllm-chat-v1` profile normalizes this case after validating exactly one complete
call, the selected name, call ID, JSON arguments and any requested strict schema.
It preserves usage and other response metadata, including integer precision.

The same rule applies to a streamed terminal event after collecting the call
fragments. Missing `[DONE]`, missing requested usage, truncated arguments,
invalid schemas and `length` endings still fail. Streaming data remains
provisional until `[DONE]`; a terminal chunk does not establish receipt success.
The profile and model are fixed when each request is admitted.

Omitted/empty `runtime_profile` retains the strict unadapted contract when tool
calling is disabled. The `hermes` parser requires `vllm-chat-v1`; incomplete
combinations and unknown profiles are rejected before startup. This profile
name is a wire-behavior selector, **not** an
image version pin or a claim that all vLLM releases are supported. Prepare each
CUDA/ROCm image separately with its actual digest and check its launch options,
model/chat template and responses. The launcher only supports the documented
vLLM entrypoint; changing `runtime_url` does not add support for another engine.

## Check repository software

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -e ./sdk/python -r tests/requirements-framework.txt
make PYTHON=.venv/bin/python check
```

Docker Compose v2 is required. `make compose-check` uses only `docker compose
config --format json`, synthetic directories and image IDs. It starts no
containers and needs no Docker daemon. Missing Compose fails explicitly.
The check validates both merged vendor configurations and injects eight invalid
configurations per vendor to confirm that important isolation checks fail.

The TLS integration gate runs all three real Python clients plus the API checker
against synthetic responses, including vLLM-style named-call endings. Framework
dependencies stay in the test environment; they do not become SDK dependencies.
No model weights or GPU images are downloaded by these checks.

## Check an installed gateway

Use the installed native SDK, customer CA and project token file. In an offline
environment, install the SDK from the prepared wheelhouse first. From this
checkout, the same command works for either vendor:

```sh
python3 tools/check_gateway.py \
  --base-url https://gateway.example.internal:8443 \
  --ca-file /etc/apostille/customer-ca.pem \
  --token-file /run/secrets/apostille-project-token \
  --model qwen3-4b --vendor nvidia --require-tools
```

For AMD, use `--vendor amd`. This is a report label supplied by the operator;
the script does not detect a GPU. Without an installed SDK, set
`PYTHONPATH=sdk/python/src` and use an environment with the SDK dependencies.

The checker runs capability/model discovery, text completion, streaming with
usage, strict JSON Schema output, named and required function calls, a synthetic
tool-result roundtrip and a named-call stream. It never executes a model-selected
tool, retries a request, enables receipts, installs assets or logs content. It
prints status-only JSON and exits nonzero on a failed check. A model must finish
these short requests within the configured output limit; a truncated answer
does not pass. Review per-check failures using synthetic data only.

Use `--require-tools` for a tool-capable deployment. If tools are intentionally
disabled, omit the flag: those checks report `not_enabled`, and a pass covers
only the remaining features. A successful report always retains
`hardware_acceptance: "unverified"` and `gpu_identity: "not_observed"`.

Keep the report alongside the independently recorded GPU, driver, image digest,
model manifest and gateway revision. Then complete cancellation/overload,
receipt verification, confidentiality, IPv4/IPv6/DNS/proxy denial, cold restart,
update/rollback and performance tests in [the P4 checklist](SUPPORT_MATRIX.md#p4-acceptance-gates).
The API checker does not replace those gates or establish model answer quality.

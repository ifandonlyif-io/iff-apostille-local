# Apostille Local

**Experimental software preview for a customer-controlled inference gateway.**
It provides a narrow text chat API, project-scoped access to an administrator's
model catalog, an optional signed metadata receipt, and a Python SDK.

This repository is a separate product prototype. It does not change IFF's hosted
x402 monitor, publish inference activity to a public transparency log, or modify
Apostille Core 0.1. It is not a validated pharmaceutical system.

## What is available

| Surface | Preview scope |
| --- | --- |
| Inference | `GET /v1/models`, `POST /v1/chat/completions`, text only, one choice |
| Streaming | SSE chunks and `[DONE]`; disconnect cancels gateway/backend work |
| Selection | Project-authorized active model; one active model per runtime deployment |
| Structured output | Strict schema with a bounded keyword subset |
| Customer integrations | OpenAI Chat Completions subset, project-scoped capability discovery, client-executed function calls |
| Evidence | Opt in using `X-Apostille-Record: metadata`; authenticated retrieval and exact-byte download by run ID |
| Workflow evidence | Optional local CLI/SDK for signed workflow assertions, explicit receiver policy and offline archive verification |
| SDK | Python 3.11+, sync/async, streaming context managers, token-file authentication |
| Deployment | Operator-supplied pinned runtime image and pre-staged model artifacts |

The receipt signs the gateway's metadata assertion. It **does not prove** prompt
or completion bytes, actual model execution, model quality, organization identity,
or absence of data exfiltration. Embedded keys establish signature integrity;
receivers need independently supplied exact key pins for trust.

## Start here

1. Read [scope and threat model](docs/THREAT_MODEL.md).
2. Follow [deployment instructions](deploy/README.md) to supply approved model
   files, exact image digest, project key hash, TLS material and signing key.
3. Use the [Python SDK guide](sdk/python/README.md) or the
   [OpenAPI specification](api/openapi.yaml).
   For a customer-owned framework, start with the [integration contract and
   official OpenAI SDK example](docs/INTEGRATIONS.md).
   Use the [runtime compatibility and API acceptance guide](docs/RUNTIME_COMPATIBILITY.md)
   for the shared AMD/NVIDIA validation commands.
4. Complete [hardware acceptance](docs/SUPPORT_MATRIX.md) before offering a
   hardware or latency commitment.
5. Follow [administrator operations](docs/OPERATIONS.md) and
   [release checklist](docs/RELEASE.md) for maintenance.
6. For customer-owned training or model-delivery workflows, see
   [universal workflow evidence](docs/WORKFLOW_EVIDENCE.md) and the optional
   [Flower CPU example](integrations/flower/README.md). These use a separate
   event profile and archive; the gateway does not become a training service.

The repository requires Go 1.25+ (toolchain pinned by `go.mod`). For SDK work:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -e ./sdk/python -r tests/requirements-framework.txt
make PYTHON=.venv/bin/python check
```

`make check` runs Go tests, race detection, vet, SDK tests, deployment and API
checker tests, both vendor Compose renders, real gateway integration with the
native SDK, pinned OpenAI and LangChain clients, and command builds. Docker
Compose v2 must be installed; its configuration check needs no running daemon.
Tests use synthetic data and a test runtime server. Individual checks are available:

```sh
make test race vet
make PYTHON=.venv/bin/python sdk-test integration
make PYTHON=.venv/bin/python deployment-test compose-check acceptance-test
make fuzz
```

`make check` also exercises the generic Python workflow recorder against the
real Go signing/verifying CLI. Optional `make flower-test` uses the pinned
dependencies in `tests/requirements-flower.txt` to test actual Flower callbacks
and aggregation with synthetic CPU data. Flower is not a gateway or native SDK
runtime dependency. Its example does not establish multi-site privacy or GPU
support; see [the integration guide](docs/WORKFLOW_EVIDENCE.md).

For a local software preview bundle, install build tools into the virtual
environment and follow [RELEASE.md](docs/RELEASE.md). See
[implementation status](docs/IMPLEMENTATION_STATUS.md) for phase gates and handoff.

## Limits and current readiness

- No automatic downloads during inference, cloud fallback, arbitrary remote
  model code, RAG, tool execution, fine-tuning, or public management API.
- Model administration is an operator task. A client cannot download or activate
  a new model through the inference API. `GET /v1/models` lists only the active
  model when authorized for the project and visible in runtime readiness checks.
- Prompt/completion content is not persisted by the gateway. Clients and runtime
  processes still handle it in memory and must meet the customer's retention policy.
- P4 / real deployment acceptance is **not verified**. The initial development
  environment has no running Docker daemon or test GPU. Mock-runtime tests and
  configuration checks are software evidence only.
- AMD/NVIDIA integration is a deployment candidate, not a tested device claim.
  Vendor sponsorship or open-source licensing supplies no product SLA.
- This preview has not been published to a package registry or shipped as a
  supported customer appliance.

IFF code is [MIT licensed](LICENSE). Runtime images, drivers, model weights and
dependencies retain their own licenses. See the [dependency inventory](docs/NOTICES.md)
and [release/SBOM requirements](docs/RELEASE.md).

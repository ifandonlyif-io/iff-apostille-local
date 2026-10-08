# Implementation status and handoff

Date: 2026-10-08. Status describes the initial open-source software preview.
Customer rollout and GPU qualification remain separate acceptance gates.

## Phase status

| Phase | Scope | Status | Remaining gate |
| --- | --- | --- | --- |
| P0 | Repository, configuration, API and safety contracts | ✅ Software foundation verified | MIT source ready; package-registry releases remain separate actions. |
| P1 | Pinned deployment and local model administration | 🟡 Software implementation only | **Docker deployment and real GPU inference have not run.** Qualify exact hardware/image/model tuple. |
| P2 | Inference gateway and Python SDK | ✅ Software contracts verified | Actual vLLM/GPU inference remains part of P4. |
| P3 | Opt-in metadata evidence and offline verifier | ✅ Software contracts verified | Provision customer keys and independent receiver pins during deployment. |
| P4 | Hardware and customer acceptance | ⬜ **Not verified** | Real deployment, egress, workload, recovery and customer acceptance in SUPPORT_MATRIX.md. |

The initial development machine has no running Docker daemon or test GPU.
Synthetic/mock runtime success is not AMD/NVIDIA hardware evidence. P1 can have
working configuration tooling while its deployment acceptance remains open.

## Implementation contracts

1. **Separate product:** this repository is independent of IFF's hosted x402
   monitor; inference and receipts do not feed public cards/logs/reputation.
2. **Local backend:** one configured local runtime and active model; no cloud
   fallback, inference-time downloads, remote model code or public management API.
3. **Project authorization:** bearer tokens come from operator-named files;
   configuration stores token hashes and model/concurrency policy. Evidence
   requires the same project's authorization. Run IDs are not capabilities.
4. **Narrow API:** text roles are system/user/assistant and tool results; one
   choice; only the fields in OpenAPI are accepted. Function-call messages allow
   customer-owned frameworks to execute approved tools outside the gateway.
   The active model's operator-approved parser gates tool calling. Structured
   output and tool parameters use a bounded schema subset, with references,
   regex/applicators and remote schema loading disabled.
5. **Streaming:** validated chunk envelopes are forwarded as SSE. Deltas remain
   provisional until the final schema check and `[DONE]`. Cancellation propagates;
   clients must not treat a truncated stream as success or automatically retry.
6. **Content handling:** prompts/completions and raw backend errors are not
   persisted or logged by the gateway. Runtime and customer application logging
   require separate controls. TLS is required outside explicit loopback tests.
7. **Metadata evidence:** recording requires `X-Apostille-Record: metadata`.
   Receipts are retained for 24 hours; ready/pending/failed is separate from
   inference success. Producer-only Core 0.1 bytes retain their existing semantics.
   Ready responses include Base64 originals; SDK downloads preserve exact bytes
   for offline verification rather than reserializing response objects.
   Trust needs an independent exact producer key pin; no output/execution claim.
8. **Release claims:** software inventories and license files cover their stated
   components. Runtime image, driver, model, OS and actual hardware require their
   own inventory and validation. This preview is not a pharmaceutical validation.

## Handoff commands

Use a local checkout with Go available. Create an isolated Python environment:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -e ./sdk/python build packaging -r tests/requirements-framework.txt
make PYTHON=.venv/bin/python check
make fuzz
make PYTHON=.venv/bin/python preview
```

`make check` requires the Docker Compose v2 CLI and runs software checks and live
SDK/gateway integration against a synthetic runtime. Its Compose checks do not
require a daemon. `make preview` builds a local bundle and never validates a GPU
or publishes it. See [deployment](../deploy/README.md) for the actual operator
commands and model/runtime prerequisites; do not substitute invented image digests.

The Python SDK requires Python 3.11+. Tests passed on Python 3.14.4 and Python
3.11.9 with httpx 0.28.1. A fresh Python 3.11.9 environment installed the SDK and
all seven dependencies from the preview's local wheelhouse using `--no-index`
and the hashed lock, then passed all 24 SDK tests. This verifies offline package
installation on the development host, not offline Linux/GPU deployment.

## Final integrated validation record

Recorded on the macOS arm64 development host using Go 1.26.6. Docker Compose
configuration parsing used synthetic paths/digests and did not start containers.

| Check | Result | Evidence / remaining work |
| --- | --- | --- |
| Go tests | Passed | `go test ./...`; real gateway process tests cover TLS enforcement, SIGTERM drain, cancellation and timeout. |
| Go race detector | Passed | `go test -race ./...`; additional process and TLS integration race checks passed. |
| Go vet / command builds | Passed | All three commands build for the host and Linux amd64; Linux binaries were not executed here. |
| Python SDK suite / installed-wheel suite | Passed | 24 tests, including byte-preserving evidence export, private CA and cancellation; fresh offline-installed wheel verified on Python 3.11.9. |
| Real TLS SDK → gateway integration | Passed | Sync/async inference, multiline SSE, schema validation, cancellation, project isolation, downloads and CLI verification; synthetic backend only. |
| Deployment tooling/configuration tests | Passed | Go admin tests, 8 Python deployment tests and both actual `docker compose config` parsers. Fake executor tests cover switch, rollback and failure recovery. |
| Fuzz | Passed | 30 seconds, 106,224 executions, no crasher (strict JSON parser). |
| Go / Python dependency scans | Passed | `govulncheck v1.1.4`: no vulnerabilities after x/text update; `pip-audit 2.10.1` on the complete seven-package hash lock: no known vulnerabilities. |
| OpenAPI / document checks | Passed | OpenAPI 3.1 validated; local Markdown links and source whitespace checked. |
| Local preview / software SPDX / notices | Built | `dist/0.1.0-alpha.1/`: binaries, SDK wheel/sdist, offline dependency wheels, hashes, module/SDK SPDX and license copies. Actual runtime/driver/model inventories remain outstanding. |
| Docker deployment / GPU inference | **Not run** | P1 deployment gate and P4 hardware acceptance open. |
| Customer acceptance | **Not run** | Agree intended use and complete SUPPORT_MATRIX.md. |

Evidence tests reject artifact tampering and wrong independent producer pins.
Synthetic input/output/token markers are absent from stored metadata and exported
records. Store tests cover cross-project reads, 24-hour expiry, ENOSPC, single
writer locking and recovery of unfinished records. Real runtime/proxy/host log
confidentiality and network denial still require the P4 tests.

## Review corrections included

- Honor the full configured inference timeout before non-streaming response headers.
- Preserve multiline SSE and reject unsupported/case-aliased request fields, null
  scalars and explicit zero token limits.
- Export exact manifest/bundle bytes for verification without SDK cryptography.
- Keep terminal receipt failures explicit across disk errors and process restarts.
- Prevent inherited Compose variables from overriding pinned model activation;
  reject FIFO/symlink assets and sync atomic configuration changes durably.
- Bind offline image archives to both the upstream repository digest and the
  loaded image config ID; Docker save/load need not preserve RepoDigests.

## Next operator decisions

Select the initial hardware and supported model revision, obtain real runtime
image digests/model artifacts, define the customer access and retention policy,
provision TLS and external evidence pins, and schedule P4 acceptance. Do not mark
the prototype customer-ready until those gates have evidence.

## Customer framework interface update

The [integration contract](INTEGRATIONS.md) documents the Chat Completions subset,
project-scoped capability discovery, streaming usage and function-call exchange.
Customers install and operate their own frameworks and tool sandboxes. No AMD or
NVIDIA agent framework is a gateway dependency. The official OpenAI Python client
is a pinned test/example dependency, separate from the Apostille Local SDK's
runtime dependencies.

Interface tests use synthetic model responses. They establish wire behavior,
authorization and failure handling; they do not certify GAIA, NeMo, OpenShell,
model tool quality, a vLLM image or any GPU. The original validation record above
remains a record of the initial preview.

This interface update was checked on 2026-10-08:

- `make check`: Go tests, race detector, vet, host command builds, 37 Python SDK
  tests, 9 deployment tests and TLS integration all passed.
- TLS integration uses both the native SDK and official `openai==2.29.0` with
  `httpx==0.28.1`. It covers capability/model isolation, sync/async text, usage
  chunks, function-call roundtrips, cancellation and sanitized errors. Both
  modes of the shipped OpenAI client example ran against the gateway.
- Tool-call receipts passed offline verification; synthetic prompt, completion,
  tool-result, function-name, argument and token markers were absent from stored
  metadata and exported evidence.
- Runtime field-alias and legacy-function rejection received additional gateway
  race/vet and TLS integration checks after the full gate; all passed.
- OpenAPI 3.1 validation, request/response schema cases, Markdown links and
  `git diff --check` passed. Release bundles were not rebuilt or published by
  this interface update; re-run `make preview` before distributing new artifacts.

## vLLM profile and dual-vendor software gate — 2026-10-09

The [runtime compatibility guide](RUNTIME_COMPATIBILITY.md) is the entry point
for the upstream references, explicit profile and customer API acceptance command.

- Added administrator-selected `runtime_profile: "vllm-chat-v1"`, pinned in the
  asset manifest. Validated named-call `stop` endings normalize to `tool_calls`
  for both ordinary responses and SSE; the empty profile preserves strict behavior.
  Invalid arguments, incomplete streams and unsupported profiles still fail.
- Added actual LangChain `ChatOpenAI` tests with `langchain-openai==1.1.11` and
  `langchain-core==1.2.18`, alongside native SDK and `openai==2.29.0` tests.
  Test dependencies remain separate from the SDK and its offline wheelhouse.
- Added daemon-free `make compose-check` for AMD/NVIDIA templates, including
  non-root/read-only execution, no gateway GPU/socket access, offline flags,
  vendor device selection, private runtime networking and pinned image selection.
  Both services explicitly target Linux amd64. Eight injected regressions per
  vendor were detected; this did not start containers.
- Added `tools/check_gateway.py`: project-authenticated TLS discovery, text,
  streaming usage, schema output, named/required calls and a synthetic tool
  roundtrip. It prints status-only JSON, executes no tools, stores no content
  and always leaves hardware acceptance unverified. Both vendor declarations
  were exercised against the synthetic TLS integration server.

Final `make check` passed: all Go packages, race detector, vet, three host command
builds, **37 SDK tests, 13 deployment tests, 17 acceptance-checker tests**, both
Compose renders and native/OpenAI/LangChain TLS integration. Acceptance checks
also remain active under optimized Python. OpenAPI 3.1, local Markdown links,
Python test-environment dependency consistency and `git diff --check` passed.
An independent review found no actionable regression in the profile/adapter
and streamed-tool validation scope.

No Docker service, actual vLLM image or GPU inference ran. P1 deployment and P4
hardware acceptance remain open. No release bundle was rebuilt or published by
this update. Obtain exact hardware/image/model pins and run the documented
customer acceptance before making a hardware support commitment.

## Universal workflow evidence and Flower preview — 2026-10-09

The [workflow guide](WORKFLOW_EVIDENCE.md) defines a framework-independent event
and receiver-policy contract. The first [Flower adapter](../integrations/flower/README.md)
uses the same contract for manufacturing and pharmaceutical synthetic scenarios.
There are no industry-specific fields in signed events.

- Added `apostille-workflow` for dedicated key creation, metadata signing,
  independent-policy verification and archive sequence checks. It reuses the
  released Core 0.1 dependency without changing Core bytes or Go dependencies.
- Added the Python `WorkflowRecorder` and explicit receipt outcomes, with no new
  native SDK runtime dependencies. Private archives, per-archive locking,
  verified sequence recovery, monotonic terminal rounds and verification before
  publication handle receipt failures separately from training results.
- Added an optional Flower 1.39.0 / NumPy 2.2.6 example: three synthetic sites,
  three CPU rounds, real `ClientApp` and `FedAvg`, with in-process transport.
  Default output is 13 signed metadata receipts. Explicit synthetic release
  approval adds a model file plus release/acceptance receipts, for 15 total.
- Receipts exclude samples, gradients, metrics, prompts, outputs and free-form
  errors. Model-byte binding requires an explicit release/acceptance operation.
  Independent pins and event permissions are checked offline. Archive completeness,
  actual execution, content truth and current authorization remain unknown.
- Cross-layer review corrected stale-round handling, policy-context checks,
  same-filesystem publication, adapter/recorder configuration matching and
  Flower reply run/request/task correlation. Framework checkpoints and a single
  training owner are still required; receipts do not guarantee exactly-once work.

Final software verification on the macOS arm64 host:

| Check | Result |
| --- | --- |
| `make check` | Passed: Go tests/race/vet, 55 SDK tests, 11 real Go CLI workflow tests, 13 deployment tests, 17 API checker tests, both Compose renders, native/OpenAI/LangChain TLS integration and four command builds. |
| `make flower-test` | Passed: 19 tests, zero skips, including both signed scenarios, explicit release, artifact tampering, cancellation/failure, restart replay and reply correlation. |
| `make security` | `govulncheck v1.1.4`: no vulnerabilities found. |
| CI workflow lint | `actionlint v1.7.7` with ShellCheck v0.11.0 passed. |
| Local preview | Built under `dist/0.1.0-alpha.1-workflow-preview/`: four Linux amd64 binaries, generic SDK wheel/sdist, schemas, checksums, software SPDX and notices. No registry publication. |
| Installed SDK wheel | Offline installation into a fresh host Python 3.14 environment passed all 55 SDK tests; the workflow module was imported from the installed wheel. |

The signed Flower tests block Python socket and DNS calls. This establishes the
single-process example's behavior; it is not kernel-enforced network isolation
or multi-host acceptance. Flower dependencies remain outside the native SDK
wheelhouse and software SPDX. A customer Flower package needs its own full
dependency inventory, hashes, notices and scan.

No distributed Flower deployment, GPU training, secure aggregation, differential
privacy, NVIDIA FLARE/OpenFL adapter or pharmaceutical validation was delivered.
No model was activated in the inference gateway. Workflow archives have no
automatic expiry; customers must define retention and access policy. Existing
P1 deployment and P4 hardware gates remain open.

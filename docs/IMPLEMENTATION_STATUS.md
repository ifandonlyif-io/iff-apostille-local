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
4. **Narrow API:** text roles are system/user/assistant; one choice; only the
   fields in OpenAPI are accepted. Structured output uses a bounded schema subset,
   with references, regex/applicators and remote schema loading disabled.
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
.venv/bin/python -m pip install -e ./sdk/python build packaging
make PYTHON=.venv/bin/python check
make fuzz
make PYTHON=.venv/bin/python preview
```

`make check` runs software checks and live SDK/gateway integration against a
synthetic runtime. `make preview` builds a local bundle and never validates a GPU
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

# Support matrix and acceptance record

## Current status

| Configuration | Status | Evidence |
| --- | --- | --- |
| Gateway with synthetic/mock runtime | Software checks only | Run repository Go checks. |
| Python sync/async SDK with MockTransport | Software checks only | Run SDK unittest suite. |
| Docker Compose deployment | **Not verified / P4 open** | Initial environment has no running Docker daemon. |
| NVIDIA GPU + pinned runtime + approved model | **Not verified / P4 open** | No real GPU acceptance run. |
| AMD GPU + pinned runtime + approved model | **Not verified / P4 open** | No real GPU acceptance run. |
| Customer pharmaceutical environment | **Not validated** | Requires agreed intended use and customer acceptance. |

This table must not become a device-support claim based only on a vendor's
compatibility page. No latency, throughput, maximum model size or user capacity
has been established for this prototype.

## Record one row per tested deployment

Capture date, operator, customer-approved test scope, hardware SKU/count and memory,
firmware, OS/kernel, GPU driver, runtime image digest, gateway revision, model
revision/file-manifest digest, precision, context length, output limit,
concurrency, TLS boundary, egress controls and acceptance results.

The exact chosen runtime's published hardware matrix is a prerequisite. AMD
ROCm and NVIDIA CUDA are distinct software builds. Driver support alone does
not prove model/quantization/engine support. A container still depends on host
drivers. Do not promise arbitrary customer models based on model catalog support.

## P4 acceptance gates

1. Start, restart and run inference without Internet or model downloads; observe
   both application and host/container network boundaries.
2. Validate TLS, project authorization, denied cross-project evidence access,
   body/schema limits, inactive/unauthorized model errors and no secret/content logs.
3. Exercise nonstreaming and streaming, long prompts within policy, structured
   output, cancellation, backend failure, timeouts, overload and slot release.
4. Measure cold start, time to first token, p50/p95 completion latency, throughput,
   memory and sustained behavior at the agreed concurrency. Use representative
   input/output lengths and measure power if making operating-cost claims.
5. Verify available receipts independently with external pins; check completed,
   failed and cancelled outcomes. Do not infer output binding from metadata.
6. Complete backup/restore, key rotation, update and rollback rehearsals.
7. Record actual results, failures and acceptance decisions. No placeholder PASS.

Model quality is a separate acceptance dimension. Agree a representative test
set and evaluation with the customer. Platform reliability does not establish
that a selected model gives reliable pharmaceutical advice.

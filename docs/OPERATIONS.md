# Administrator operations

Follow [deployment instructions](../deploy/README.md) for executable commands.
The administrator interface is local CLI/configuration, not a network management API.

## Configuration contract

Configuration is strict JSON with `version: 1`; unknown fields fail validation.
Use the repository example as a template and supply real values locally.

| Field | Operator responsibility |
| --- | --- |
| `listen`, `tls_cert_file`, `tls_key_file` | Bind the intended interface and supply customer-trusted TLS material. |
| `runtime_url` | A dedicated local HTTP runtime (`runtime`, localhost, loopback or private IP); do not expose its port to client networks. |
| `active_model` | One configured model selected for this runtime. Change through controlled administration. |
| `models[]` | `id`, immutable `revision`, `manifest_sha256`, `license`, `runtime_image` by `@sha256:`, `precision`, `max_context`, `max_tokens`, `max_concurrent`, local `path`. |
| `projects[]` | Unique `id`, high-entropy key's `api_key_sha256`, authorized `models`, `max_concurrent`. |
| `evidence` | Private `directory`, `key_file` (ML-DSA-65 Apostille JSON key file; an Ed25519 key signs classical Core 0.1), `agent_id`, optional `require_post_quantum` (refuse any key that does not sign Core 0.3). An empty directory disables evidence storage. |
| `timeout_seconds` | Request budget from 1 to 600 seconds. |

Current configuration supports `bfloat16` and `float16`; adding a model to the
catalog does not add support for arbitrary quantization formats. Revisions must
be 40–64 lowercase hexadecimal characters, and SHA-256 values exactly 64.
`max_tokens` must be positive and below `max_context`; model/project concurrency
must be 1–32. These validation bounds are not hardware sizing recommendations.

## Before first use

1. Obtain the model under its actual license; save license/attribution alongside
   the model manifest. Verify every intended artifact and the runtime image digest.
2. Generate project tokens with the operator tool. Deliver raw tokens through the
   customer's secret channel; configure only hashes, use restricted token files,
   and never paste tokens into tickets or shell history.
3. Keep the evidence signing key separate from TLS and project authentication
   credentials. Back it up in the customer's approved secret store.
4. Provision clients with TLS trust and independent evidence key pins. Do not ask
   clients to disable HTTPS verification to get a demo working.
5. Restrict the runtime and evidence storage to service identities. The gateway
   must not receive a Docker socket, administrative host mount or cloud credentials.
6. Disable runtime content logs, external traces, automatic downloads and remote
   code. Apply egress controls and test them on the actual deployment.

## Model changes and customer choice

The configured catalog is administrator-managed. The inference API lists only
the active, authorized model when runtime readiness succeeds; other authorized
model IDs return an inactive-model error. Treat a model change as an administrative release:
stage immutable artifacts, verify hashes/licenses, drain active requests, select
the model, restart the matching runtime/gateway, run smoke checks, then reopen
traffic. Preserve the previous configuration and artifacts for rollback.

A supported combination is a tuple of model revision, format/precision, runtime
image digest, driver, GPU, OS/kernel, context and concurrency. Changing any member
requires appropriate revalidation. Model aliases or mutable image tags do not
preserve that tuple.

## Keys, retention and recovery

The [SDK evidence download](../sdk/python/README.md#download-and-independently-verify-evidence)
exports exact manifest/bundle bytes into a new private directory. Verify those
files with `apostille-local-verify` and an independently provisioned producer pin;
never turn an embedded/API-fetched key into a trust anchor. Reserializing the API's
JSON object fields can change the artifact bytes and break verification. Exported
files require their own retention policy; downloading does not extend server retention.

- Rotate project tokens by replacing the configured hash and distributing the
  replacement file; restart/reload only as supported by the deployed binary.
  Existing SDK objects cache tokens and must be recreated.
- Rotate evidence signing keys independently. Keep a dated receiver pin policy
  and old public pins needed to verify retained receipts.
- This preview retains records for a fixed 24 hours; expired records are no longer
  returned and the service's purge loop removes them. This is not configurable
  through the current JSON contract. Confirm this retention and access policy
  before enabling recording. Backups need their own deletion policy. Metadata
  can still reveal usage patterns and model/project identities.
- Back up approved configuration, evidence and required key material separately.
  Encrypt backups and test restore using synthetic records.
- On failure, preserve sanitized status/run identifiers. Never attach request
  bodies, completions, token files, signing keys or raw backend logs to support cases.
- Stop serving if artifact integrity or credential isolation is uncertain. Restore
  a known configuration and re-run acceptance; do not silently use another backend.

## Pharma customer boundary

The preview supplies an inference platform. It does not supply a validated GxP
workflow, batch-release decision system, clinical tool or regulatory certification.
Agree the intended use, customer QA responsibilities and acceptance evidence
before use in a regulated process. SDK tests and a signed metadata receipt do
not replace that work.

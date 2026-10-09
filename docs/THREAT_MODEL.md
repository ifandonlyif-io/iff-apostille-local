# Threat model and evidence scope

## Intended boundary

A customer controls the host, local model files, private inference network,
project credentials, TLS termination, signing key and retained metadata. The
gateway accepts authenticated text requests, authorizes a selected model, calls
one configured local runtime and optionally records metadata evidence.

Protected assets are customer text, generated text, model artifacts, project
tokens, signing material and isolation between projects. The application does
not require a cloud inference provider. No request is automatically sent to a
different model/provider after a failure.

## Trust and limitations

- The operating system, administrators, runtime, GPU driver and inference model
  are trusted with plaintext. A compromised host can read or change them.
- TLS protects a configured network path. Deploying on premises alone is not
  proof that egress is impossible. Deny egress at the host/network boundary and
  test the exact deployment, including container forwarding rules.
- Project tokens are bearer credentials. The configuration stores SHA-256 token
  digests; generate high-entropy tokens and protect raw token files. A digest is
  not a substitute for sufficient token entropy. `POST /v1/messages` also
  accepts the same token in `X-Api-Key` (exactly one of that header or
  `Authorization: Bearer`), with the same hashing and constant-time comparison.
- Model files and runtime images are supply-chain inputs. Verify manifests,
  fixed revisions, licenses and image digests before activation. Do not accept
  runtime downloads, remote model code, or unreviewed plugins.
- The runtime receives content in memory. Operators must disable runtime prompt
  logging, debug traces, core dumps and external telemetry. The gateway cannot
  enforce every runtime's retention behavior.
- Resource limits constrain request size, schema shape and concurrency. Global
  and project slots cover body reading, validation/schema compilation and runtime
  inference; saturation returns 429 before parsing another body. They do not
  establish a GPU throughput or denial-of-service guarantee.
- Function calls are untrusted model output. The gateway validates their wire
  format and, for strict definitions, argument schema; the customer application
  must independently authorize each action and constrain its tool sandbox.
  No function is executed by the gateway. Customer frameworks, callbacks, tool
  results, history and trace stores require their own content-retention controls.

## Signed metadata semantics

Recording is opt-in per request via `X-Apostille-Record: metadata`. Ordinary
requests do not authorize recording prompt/completion content. Run IDs are
correlation identifiers, not authorization credentials.

An evidence response can be `pending`, `ready` or `failed`. `ready` means a
metadata receipt is available; it is separate from the inference outcome. A
cancelled or failed inference must never be relabeled successful simply because
its receipt was signed. Receipt failure does not justify retrying inference.

The Core 0.1 producer signature proves that the holder of an embedded key signed
a metadata assertion. It does not prove exact output content, execution of a
particular model, physical GPU identity, non-exfiltration, factual correctness,
or clinical/pharmaceutical suitability. Model identifiers, revisions and digests
are declared/configured metadata unless independently checked.

Offline verification must not fetch keys, schemas, current status or other
evidence. Trusted attribution needs an independently provisioned exact key pin;
the embedded key and any API-returned pin are not independent trust sources.
Rotation and historical trust policy belong to the receiver.

A `tool_calls` finish reason records completion of the model's generation step.
It does not establish tool execution, tool authorization or completion of an
agent workflow. Function names, arguments and results are not included in the
metadata receipt. Use a verifier from the same reviewed release/source revision
as the gateway; older preview verifiers that accept only `stop` and `length`
reject these new metadata records. Apostille Core 0.1 signatures are unchanged.

## Abuse cases to exercise before a customer pilot

1. Another project's token cannot see unauthorized models or retrieve run evidence.
2. Invalid credentials, malformed JSON/schema, oversized requests and unavailable
   models return bounded errors without content or secrets.
3. Backend failures and malformed SSE never echo raw error bodies or synthesize a
   successful completion. Client cancellation releases runtime work and slots.
4. An offline host cannot resolve/reach public model hubs or cloud APIs during
   start, inference, model switching, telemetry and restart.
5. An unprivileged API caller cannot activate/download a model, inspect raw
   storage, configure the runtime or access container management.
6. Restores, key rotation and update rollback preserve metadata access boundaries.

Use synthetic text for repository tests. Evaluate customer-confidential cases
only inside their approved environment. Product claims must remain within the
actual [acceptance record](SUPPORT_MATRIX.md).

## Optional workflow evidence boundary

The standalone workflow CLI and Python recorder follow
[WORKFLOW_EVIDENCE.md](WORKFLOW_EVIDENCE.md). Their archives are operator-owned
local files with no automatic expiry or HTTP access service. Project/job/agent
and event permissions are checked against an independently supplied receiver
policy. Process locks coordinate recorder writers; they cannot stop an authorized
host administrator from removing files, replacing policy or using the key.
Sequence checks detect replay/gaps in the supplied prefix, not omitted suffixes
or whole producer streams. Receipt validity does not establish archive completeness,
live revocation, actual execution, differential privacy, non-exfiltration or an
organization's legal identity. Model bytes are bound only on explicit release
or acceptance; the original must be supplied separately to verify that binding.
No private sample, raw update, gradient, per-site metric or free-form log enters
the event schema. Opaque metadata, digests, public keys and timestamps can still
identify or correlate activity. The customer controls export, retention and
backup policies independently of inference records' 24-hour lifetime.

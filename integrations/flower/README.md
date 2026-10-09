# Flower workflow evidence preview

A runnable, **single-process CPU integration** of Flower 1.39.0, NumPy and
Apostille Core 0.1 receipts. Three synthetic sites train a small linear regression
model over three rounds. Flower's real `ClientApp.train` dispatch,
`FedAvg.configure_train` and `FedAvg.aggregate_train` are exercised; a small
in-process node list replaces the network transport. There is no Ray, PyTorch,
CUDA, ROCm, external model, dataset download or service to start.

This validates the adapter and receipt contract. It does not validate a distributed
Flower deployment, GPU configuration, pharmaceutical use, secure aggregation or
differential privacy. Default FedAvg gives the coordinator individual model
updates; those updates may reveal information about local training data.

## Prepare and run

From the repository root, install dependencies in a dedicated environment during
your controlled preparation stage (requires Python 3.11 or newer):

```bash
python3.11 -m venv .venv-flower
.venv-flower/bin/python -m pip install -e sdk/python -r tests/requirements-flower.txt
make build
make flower-test FLOWER_PYTHON=.venv-flower/bin/python
```

For an air-gapped machine, prepare the Python wheels and Go binary on a compatible
OS/architecture first and install the wheels using `--no-index --find-links`.
Dependency installation is a separate operator action. The demo never invokes
`pip`, `uv`, a model provider or a Flower cloud service. This is an example, not a
complete offline deployment package.

Run with a **new** output directory. The directory contains synthetic private
keys: keep it local, do not commit it, and never reuse these keys in production.

```bash
DEMO_PARENT="$(mktemp -d)"
PYTHONPATH=sdk/python/src:integrations/flower .venv-flower/bin/python \
  integrations/flower/demo.py \
  --cli "$PWD/bin/apostille-workflow" \
  --output "$DEMO_PARENT/manufacturing" \
  --scenario manufacturing
```

Expected summary: 3 completed rounds, 13 receipts, `receipt_status: ready`.
It creates one configuration approval plus 3 site receipts and 1 coordinator
receipt per round. Keys and archives have private permissions. No training inputs,
labels, individual updates, aggregate model bytes or metrics are written to the
receipt archive. The keys are separate from `export/`.

To also exercise an **explicitly authorized synthetic release and simulated
acceptance**, use a separate output directory and opt in:

```bash
PYTHONPATH=sdk/python/src:integrations/flower .venv-flower/bin/python \
  integrations/flower/demo.py \
  --cli "$PWD/bin/apostille-workflow" \
  --output "$DEMO_PARENT/pharma" \
  --scenario pharma \
  --approve-synthetic-release
```

Expected: 15 receipts. This adds a final `synthetic-model.npy` file and two
artifact-bound receipts. The deployment signer accepts it only after the CLI
checks the release signature against the receiver policy and matches the exact
file. It does not deploy the model into Apostille Local's inference gateway.

The `manufacturing` and `pharma` scenarios intentionally use the same synthetic
algorithm and generic event schema. They demonstrate that industry labels do not
change the signing contract; neither is an industry-trained model.

## What the adapter records

- `Participant.wrap` records `work_completed` only after a successful train
  callback returns a finite model and a reply for that request. Ordinary errors
  produce fixed error replies and `work_failed` receipts. Cancellation writes
  `work_cancelled` best-effort and re-raises the original interrupt/cancellation;
  it never becomes a normal reply that permits another round to run. The adapter
  never serializes callback arguments or results.
- `EvidenceFedAvg` calls Flower's implementation for aggregation. It requires all
  expected nodes exactly once, the current round, matching run/request/task routing,
  no error replies, and a finite
  aggregate before recording completion. An incomplete or failed round produces
  no success receipt. Error replies are rejected before Flower's default logger
  can print their potentially sensitive `reason` text.
- Signature status is independent of computation status. Read `last_receipt` after
  each hook. A signing failure preserves a completed result and does not repeat
  training or claim to undo it. Check `last_receipt.warning` as well: a committed
  receipt can be ready with a fixed cleanup/durability warning. The demo blocks
  release if any required receipt was not produced.
- Configuration approval, model release and deployment acceptance are explicit
  caller decisions. Successful aggregation does not automatically approve a
  release. The demo uses distinct approver, coordinator, deployer and site keys.

`WorkflowRecorder` is the reusable framework-independent SDK interface. Flower
is an optional integration dependency and is not added to the base SDK.

## Receiver trust and lifecycle

Each demo site has its own local key and receipt directory. The receiver policy
is initialized from the demo key ceremony's public pins, **not** from receipt
contents. In a real deployment, obtain pins and permissions through an independent
administrator channel. The simulated identities and local policy do not establish
real organizations, regulatory approval or external trust.

Verify a site's or the explicitly shared `export/` archive with an independently
selected policy:

```bash
bin/apostille-workflow verify-set \
  --directory "$DEMO_PARENT/pharma/export" \
  --policy "$DEMO_PARENT/pharma/receiver-policy.json"
```

Before a callback or coordinator dispatch, the wrapper calls the recorder's
`reserve_round()` to persist a local attempt high-water mark under the archive
lock. It combines signed rounds with prior unsigned reservations, rejects old or
skipped rounds and mismatched configuration IDs, and consumes the next round
before running work. The private `.attempts`/`.attempts.pending` operational files
are bounded and remain inside the archive; include them in local backups, not in
receipt exports. Valid interrupted pending writes are conservatively consumed.
A corrupt or foreign marker blocks work.

After a signing failure and process restart, that attempted round stays consumed;
an explicitly requested next round is possible. This does **not** choose or
supply the model checkpoint for resumption. A crash after reservation may have
occurred before, during or after training. The framework/operator must reconcile
its durable checkpoint and scheduler before supplying the next round and model.
Use one training owner per job/site. The archive lock is not a distributed lock
held for the whole computation, and rollback/deletion of unsigned state cannot
be detected. The adapter makes no exactly-once execution claim.

A published receipt remains `ready` when post-publication cleanup fails. The SDK
reports `cleanup_pending` or a durability warning and retries narrowly validated
cleanup on later reads, without replaying training. Unknown/uncommitted staging
still blocks resumption; see [recovery and backup details](../../docs/WORKFLOW_EVIDENCE.md#published-receipts-and-cleanup-recovery).
Do not reset attempt state or infer that a missing receipt means nothing ran.
The demo refuses an existing output directory and implements no automatic
checkpoint resumption.

The receipt describes what a key asserted about identifiers and completion.
It does not prove computation happened, model quality, absence of poisoning,
non-exfiltration, data provenance, or completeness of all expected receipts.
Optional model hashes reveal an equality relation and require the operator's
explicit approval to share. Only release/acceptance events bind model bytes.

## Integration boundary

Import `adapter` before Flower, or set `FLWR_TELEMETRY_ENABLED=0` before process
startup. The adapter sets that value and
`FLWR_DISABLE_RUNTIME_DEPENDENCY_INSTALLATION=1`; it rejects an import that is too
late to disable Flower's cached telemetry configuration. It never starts a server.
The demo initializes Flower 1.39's internal `TaskIdentity` solely to supply the
context normally provided by its runtime; this pinned harness API is checked by
the optional test suite. Production adapters let the Flower runtime own identity. The adapters require
assigned request IDs by default, retaining outgoing Message references to see IDs
assigned by Flower's Grid after scheduling. Only this in-process harness passes
`allow_in_process_messages=True` to permit unassigned IDs; do not enable that
option in a transport deployment. Upgrade the pinned integration before changing
Flower versions; the adapter rejects untested versions instead of mislabeling
signed metadata.

For actual multi-host use, the operator still owns Flower TLS/node authentication,
transport policy, approved training code, privacy controls, retention, key
provisioning and scheduler/checkpoint recovery. Metadata node IDs are scheduling
checks, not cryptographic identity proof. Audit upstream/framework logging with
synthetic markers before sending private training inputs. Review permitted egress
at the host boundary; Python environment flags alone are not a firewall.

The tests run the full signed examples with Python socket and DNS calls blocked,
reject changed model bytes, exercise failed/cancelled clients, check that a private
exception marker is absent from every generated file, and reject restart replay
before executing a callback. They do not claim kernel-enforced offline acceptance
of a multi-host Flower stack.

## Upstream references and licenses

- [Flower 1.39.0 on PyPI](https://pypi.org/project/flwr/1.39.0/)
- [Flower ClientApp](https://flower.ai/docs/framework/ref-api/flwr.clientapp.ClientApp.html)
- [Flower FedAvg](https://flower.ai/docs/framework/ref-api/flwr.serverapp.strategy.FedAvg.html)
- [Flower privacy explanation](https://flower.ai/docs/framework/explanation-differential-privacy.html)

Our adapter and example use this repository's MIT license. Flower is Apache 2.0;
NumPy is BSD 3-Clause. Both are installed dependencies, not vendored source. Retain
third-party licenses and notices when redistributing a prepared environment.

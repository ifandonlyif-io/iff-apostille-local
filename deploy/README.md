# Linux deployment and local administration

**Status: experimental; no NVIDIA or AMD configuration has passed hardware acceptance yet.**
The repository has no certified model revision, runtime digest, GPU throughput or
offline deployment claim. The CPU tests use synthetic files and a fake Docker
executor. Fill the deployment values using actual reviewed assets; blank values
deliberately prevent accidental startup.

## Boundary

One Linux host, one selected GPU, one active model. The Go gateway serves TLS on
8443 and calls an unexposed vLLM runtime on an internal Docker network. Each
project has its own API key hash and model allowlist. The runtime never sees the
TLS private key or Apostille signing key. The administrator CLI runs on the
host; the gateway has no Docker socket, privileged mode, management route or
download operation. Models are selected from the configured catalog; installing
a model and activating it are separate administrator operations.

`compose.nvidia.yml` uses NVIDIA device reservation; `compose.amd.yml` passes only
the selected render device and `/dev/kfd`, with its host device groups. Neither
file grants privileged mode, host networking, host IPC or unconfined seccomp.
These restrictions may reveal incompatibility on a particular GPU/image. Do not
silently remove them to turn a failing acceptance test into a pass.

The gateway joins an ingress bridge as well as the internal inference bridge.
`internal: true` by itself therefore does **not** block gateway internet access.
The scoped host firewall and the deny-egress acceptance tests below are required.
This template uses fixed bridge names and supports one installation per host.

## Record the qualification matrix first

Record GPU vendor/model/UUID/VRAM, CPU architecture, Linux release/kernel, driver,
CUDA or ROCm version, container runtime and Compose version, runtime upstream
repository digest and Docker image ID, model upstream revision and license,
precision, maximum context/output tokens and concurrency. Confirm that the exact
combination is supported by the pinned vLLM release. The launcher requires a
release with `--no-enable-log-requests`, `--no-enable-log-outputs` and
`--default-chat-template-kwargs` support. Its default disables Qwen thinking.
Unsupported flags fail startup; they are never dropped automatically.

Optional client-executed function calling additionally requires the pinned
runtime's `--enable-auto-tool-choice --tool-call-parser hermes` support and a
matching model chat template. These flags parse model output; they do not run
tools. Qualify tool selection, JSON arguments, streamed fragments and cancellation
on the actual model/image before enabling the feature for customers.

References: [vLLM GPU requirements](https://docs.vllm.ai/en/stable/getting_started/installation/gpu/),
[vLLM serving options](https://docs.vllm.ai/en/latest/cli/serve/),
[Docker GPU reservations](https://docs.docker.com/compose/how-tos/gpu-support/),
[Docker network isolation](https://docs.docker.com/compose/how-tos/networking/).
Pin versions in the customer's acceptance record; these live pages are not a
certification of this repository.

## 1. Prepare the offline package on a staging machine

Build administrator and gateway binaries with the repository's Go toolchain and
pinned modules. For an offline container build, prepare `vendor/` on the staging
machine with `go mod vendor`; it is a generated packaging input, not a second
source of dependency versions. Independently verify the Go builder image and
preload it, then build with `deploy/Dockerfile`, `--network=none`, and
`--build-arg GO_BUILD_IMAGE=<actual pinned builder image>`. `GOTOOLCHAIN=local`,
`GOPROXY=off`, `GOSUMDB=off` and `-mod=vendor` prevent dependency downloads during
the build. Record the resulting gateway image ID; there is no published gateway
image in this prototype.

Obtain the model snapshot and runtime image through an administrator-reviewed
staging process. Check license/redistribution rights before placing weights or
vendor images in a customer package. The CLI does not download either asset.

```sh
docker image save --output runtime.tar "$RUNTIME_IMAGE"
apostille-local-admin assets prepare \
  --source "$MODEL_SNAPSHOT" --image-archive runtime.tar --out offline-bundle \
  --id "$MODEL_ID" --revision "$MODEL_REVISION" --license "$MODEL_LICENSE" \
  --image "$RUNTIME_IMAGE" --runtime-profile vllm-chat-v1 --precision bfloat16 \
  --max-context 4096 --max-tokens 512 --max-concurrent 1
```

Function calling is disabled by default. To prepare an approved tool-capable
model, append `--tool-call-parser hermes` to `assets prepare` and retain
`--runtime-profile vllm-chat-v1`. Hermes requires that profile: both configuration
validation and the runtime launcher reject Hermes with an empty profile, because
named tool calls need the vLLM finish-reason adapter. The CLI includes
`tool_call_parser` in the model metadata and the launcher derives its parser
flags from that pinned metadata. No arbitrary parser or plugin path is accepted.
Changing the parser requires preparing a **new bundle**, reviewing its new
`manifest_sha256`, importing it and updating the catalog entry before activation.
Editing only the config field fails manifest verification. Existing bundles
without this field retain text/structured-output behavior and do not acquire
tool calling automatically. See [customer integration](../docs/INTEGRATIONS.md)
for the separate client-side tool execution boundary.

The explicit `--runtime-profile vllm-chat-v1` selects the narrowly scoped vLLM
wire adapter. It normalizes a fully validated named function call's terminal
`stop` to `tool_calls`, including streams. Omission keeps the strict unadapted
contract and is supported only with tool calling disabled. This profile is also
pinned in the bundle: changing it requires a new bundle and catalog pin. It
neither selects an image version nor enables tools.
The NVIDIA CUDA and AMD ROCm images need separate reviewed digests and matching
bundles. See [runtime compatibility](../docs/RUNTIME_COMPATIBILITY.md) for the
source evidence and acceptance limits.

`RUNTIME_IMAGE` must be a reviewed `repository@sha256:digest`, already available
in the staging Docker daemon. Preparation resolves that reference to its
immutable Docker image config ID and records both. Docker save/load may lose
repository digests; activation therefore runs the recorded image **ID**, while
the catalog and signed metadata retain the upstream digest. The image archive
is checksummed and only explicitly passed to Docker load by the host CLI.

The result is a directory with `manifest.json`, `model/`, and `image/runtime.tar`.
Only JSON, Safetensors, tokenizer `.model`/`.tiktoken`/`.vocab`/`.merges`, `.txt`,
`.md`, and attribution files named `LICENSE`, `LICENCE`, `NOTICE` or `COPYING`
are permitted. Model cards and original license/notice files are preserved and
checksummed with the weights. Python, pickle, PyTorch `.bin`/`.pt`/`.pth`, hidden files,
symlinks and traversal paths fail. A model `auto_map` requiring custom code
fails. Stage a clean, complete snapshot, including its supported chat template
in tokenizer metadata. No remote-code exception is available.

Transfer the printed `manifest_sha256` through an independently trusted channel.
Transporting an untrusted bundle together with its own hash does not establish
authenticity. Also package the reviewed gateway image, CLI binary, deployment
files, licenses/notices and installation checksums. Confirm every required image
is locally available, including any probe image used for acceptance.

## 2. Import and provision the customer host

```sh
apostille-local-admin assets import --source "$TRANSFERRED_BUNDLE" \
  --out "$INSTALLED_BUNDLE" --sha256 "$TRUSTED_MANIFEST_SHA256" --load-image
apostille-local-admin assets verify --dir "$INSTALLED_BUNDLE" \
  --sha256 "$TRUSTED_MANIFEST_SHA256"
apostille-local-admin key-create --out "$PROJECT_KEY_FILE"
```

Destinations must not exist. Import verifies before and after copying and never
extracts model archives. `--load-image` loads the local image tar and checks the
recorded image ID exists. Docker image loading is an administrator trust boundary;
only use authenticated, reviewed packages. No secret is printed by key creation:
stdout contains only `api_key_sha256`, which belongs in the project's config.
Deliver the owner-only key file to the intended local SDK client privately.

Start with `config.json.example`; its explicit `REPLACE` markers make the file
invalid until actual reviewed pins and the key hash are entered. It contains one
`qwen3-4b` catalog entry. To add `qwen3-8b`, prepare and verify a separate bundle,
copy its model metadata into a second entry, and add that ID to the intended
project's allowlist; do not copy the 4B revision/hash or assume both fit in VRAM.
Keep only one `active_model`. The names are pilot candidates, not certified pins.

Construct `config.json` using the shared Go configuration schema. Copy each
manifest's `model` fields, set `manifest_sha256` to the trusted hash and `path` to
the **absolute host path of the installed bundle**. Set `active_model` to one
catalog entry, `listen` to `0.0.0.0:8443`, and `runtime_url` to
`http://runtime:8000`. Container TLS paths are `/tls/server.crt` and
`/tls/server.key`; use a customer CA certificate with both the customer endpoint
and `localhost` as appropriate SANs. The admin readiness check verifies TLS and
uses a loopback URL; it never offers a skip-verification flag.

Empty `tool_call_parser` and `runtime_profile` strings are equivalent to omitted
fields, matching the Go manifest encoder. No other field is normalized or
ignored; nonempty options must match the pinned manifest exactly.

Optional evidence uses `directory: /evidence`, `key_file: /signing/signing-key.json`, and
an Apostille Core UUID `agent_id`. The key must be a locally generated ML-DSA-65
Apostille JSON key file (Core 0.3, post-quantum), owner-only, never an API key or
a hosted service key. Generate it with `umask 077` set first, using either
`apostille keygen --out signing-key.json --role gateway` (the Apostille CLI,
`go install github.com/ifandonlyif-io/iff-apostille/cmd/apostille@v0.4.0-alpha.1`;
its default algorithm is ML-DSA-65) or `bin/apostille-workflow keygen --out-key
signing-key.json --agent-id <uuid>`. Both write the same file format and refuse
to overwrite. An existing Ed25519 key keeps working and signs Core 0.1, which is
classical, not post-quantum: either an Apostille JSON key file or the legacy raw
32-byte seed (accepted for compatibility only; do not create new ones). Any other
file content is rejected. Set `"require_post_quantum": true` in the `evidence`
object to make the gateway refuse to start with a key that does not sign Core 0.3.
Keep evidence disabled by leaving its fields empty until the
key and receiver pin have been provisioned. Evidence is producer metadata only:
it does not prove output integrity, actual model execution, truth, or isolation.

Provision the following permissions for the template's UID/GID `10001:10001`:

| Host object | Suggested ownership/mode | Purpose |
| --- | --- | --- |
| Config directory / config file | root:10001, 0750 / 0640 | Admin writes, containers read |
| Imported model directories / files | root:10001, 0550 / 0440 | Runtime reads; admin owns lifecycle |
| TLS directory / certificate and key | 10001:10001, 0700 / 0600 | Gateway can read private key |
| Signing directory / key file | 10001:10001, 0700 / 0600 | Required owner-only key file |
| Evidence directory | 10001:10001, 0700 | Gateway writes bounded metadata |
| Administrator state directory | root:root, 0700 | Never mounted into a container |

Apply changes only to the chosen installation directories. Atomic activation
preserves the existing config owner/group/mode. Ordinary shell editors or restore
commands may not; recheck permissions after a manual operation. For disabled
evidence, use dedicated empty evidence/signing directories, not unrelated data.

Copy `deployment.env.example` outside Git and fill every applicable value.
Choose unused, distinct private IPv4 /24 networks; do not reuse a corporate LAN
or another Docker subnet. Default publishing is loopback only. A customer LAN
binding requires a deliberate interface choice, certificate SAN and inbound
firewall policy. Keep Docker and GPU administration unavailable to application
users. Disable unencrypted swap/core dumps and inspect host crash/support tooling
before using confidential data.

## 3. Establish network isolation, then activate

For the first deployment, create `state/active.env` from the selected manifest:
`APOSTILLE_RUNTIME_IMAGE='sha256:<actual recorded image ID>'`,
`APOSTILLE_MODEL_BUNDLE='<absolute installed bundle path>'`, and
`APOSTILLE_ACTIVE_MODEL='<catalog ID>'`. The admin CLI checks these fields against
the current config. These are placeholders, not usable hashes.

Use the selected vendor Compose file plus both environment files. Run
`docker compose ... config` to inspect the resolved configuration, then
`docker compose ... create --pull never` to create containers/networks without
starting them. Verify the runtime has no published port and the config is mounted
as a **directory**; mounting only the file would retain the old inode after an
atomic activation.

```sh
python3 scripts/egress_policy.py \
  --ingress-subnet "$INGRESS_SUBNET" --inference-subnet "$INFERENCE_SUBNET"
# Review the printed scoped rules while services remain stopped.
sudo python3 scripts/egress_policy.py --apply \
  --ingress-subnet "$INGRESS_SUBNET" --inference-subnet "$INFERENCE_SUBNET"
```

The helper requires Linux, the Docker iptables backend, the exact named bridge
subnets and unused `IFF_APL_*` chains. It blocks internet forwarding and access to
host services from those bridges, except established response traffic and the
private runtime's port 8000. IPv6 forwarding/host access is blocked for the same
bridges; Compose also disables container IPv6. Docker DNS cannot forward external
queries because upstream DNS is configured to the container's own loopback.
No global firewall table is flushed or default policy changed. Existing managed
chains cause refusal instead of replacement. If an apply fails, keep services
stopped and inspect the scoped chains; no isolation success is reported.

For nftables-native Docker or another network stack, have the customer network
administrator provide equivalent scoped rules and pass the same tests. Persist
the reviewed rules with the host's firewall manager **before enabling any service
at boot**. Templates use `restart: no` to avoid a reboot starting services before
the firewall. Host administrators retain the ability to change these controls.

```sh
apostille-local-admin model-activate --config "$CONFIG_FILE" --model "$MODEL_ID" \
  --compose-file "$VENDOR_COMPOSE_FILE" --env-file "$DEPLOYMENT_ENV" \
  --state-dir "$ADMIN_STATE_DIR" --ca-file "$CUSTOMER_CA_FILE"
```

Activation checks both old and new assets/images, gracefully stops the gateway
with its configured request timeout plus drain allowance, stops the old runtime,
atomically writes the new config and active environment, recreates runtime and
gateway with `--pull never`, and waits for TLS `/readyz`. The runtime verifies
all manifest file hashes again before launch. A failed startup/readiness test
restores the original config bytes and model environment and checks the old
deployment. A failed rollback leaves `activation-recovery.json` and blocks later
activation. After an interruption, keep services stopped, inspect that file,
restore the intended config and matching active environment with correct
permissions, verify assets, then start and check readiness. Delete the recovery
marker only after this manual recovery succeeds. Never edit config concurrently
with activation.

The administrator must use the same config directory/file, environment file,
Compose project and vendor file for every operation. `doctor --config ...` checks
metadata/assets read-only and reports OS, Docker/Compose and vendor inventory
gaps with remediation. Inventory successes remain `unverified`; they do not run
inference or certify the GPU. Inherited `APOSTILLE_*`/`COMPOSE_*` variables are
removed from subprocesses so a stale shell export cannot override reviewed env
files. Only a local Unix Docker socket is accepted. Config mount paths must be
literal entries matching the supplied config file.

## 4. Actual offline and no-egress acceptance

Run the [synthetic API checker](../docs/RUNTIME_COMPATIBILITY.md#check-an-installed-gateway)
against the deployed TLS gateway for either vendor. It tests the same public
contract and prints status-only JSON. Its `requested_vendor` is an operator
label; a passing report cannot identify or qualify the physical GPU.

Before customer traffic, run a synthetic request on the exact hardware and
record warmup, peak memory, context/concurrency limits, timeout/cancel behavior,
text/JSON-schema/streaming behavior, switch/rollback and cold restart results.
For an enabled tool parser, also exercise complete client-side tool roundtrips,
strict schema rejection, streamed arguments and cancellation before tool execution.
Repeat with external connectivity physically/firewall blocked. A missing model,
tokenizer, runtime image or compile-time asset must cause failure, never a fetch
or cloud fallback. Runtime writable caches are tmpfs; qualify a cold start with
empty caches. An offline package is incomplete until that real test passes.

`egress_probe.py` sends connection attempts only; it contains no customer data:

```sh
python3 scripts/egress_probe.py --expect allow > "$CONTROL_REPORT"
# For each gateway and runtime container ID, using an already loaded image with Python:
docker run --rm --pull never --network "container:$SERVICE_CONTAINER_ID" \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,src=$ABSOLUTE_EGRESS_PROBE,dst=/probe.py,readonly" \
  --mount "type=bind,src=$CONTROL_REPORT,dst=/control.json,readonly" \
  --entrypoint python3 "$PINNED_LOCAL_PROBE_IMAGE" /probe.py --expect deny \
  --control-report /control.json
```

The host check is a positive control for IPv4 and external DNS; if it fails, the
test is inconclusive. IPv6 without a successful host IPv6 positive control is
reported `unverified_no_positive_control`, even when the deny attempt fails.
For a customer proxy, add the same `--proxy-host HOST --proxy-port PORT` to both
checks; its reachable host positive control is required. Do not pass credentials.
An unspecified proxy remains unverified. Run the deny probe in **both** namespaces.
Check IPv6 and direct connections as well as DNS. The scripted probes cover named
paths, not every possible channel: review firewall rules, capture traffic during
synthetic inference, and test customer-designated canary destinations too.
Re-run after Docker, kernel, firewall or deployment changes. None of these checks
have run on customer hardware as part of this repository implementation.

Runtime stdout/stderr are redirected to `/dev/null` before vLLM starts, request
and output logging are disabled, and Docker logging is disabled. Thus exception
traces are not recoverable through `docker logs` or `docker attach`. Gateway health
codes remain available. Debugging a startup requires a separate synthetic-only
qualification environment; do not turn on raw runtime logs with customer data.

## API key rotation and revocation

Generate a new key file with `key-create`, deliver it privately, and schedule a
cutover. Replace the project's `api_key_sha256` with the new hash. This version
accepts one key per project; there is no overlapping dual-key window. Gracefully
stop and recreate **gateway only** using the same Compose/env files, wait for
TLS readiness, then verify the old key returns 401 and the new key succeeds.
Deleting a project revokes its access after the same restart. If removing the
last project, stop the service instead; the config deliberately rejects an empty
project list. In-flight work drains within the configured timeout; revocation is
not retroactive to a request already admitted. Keep the old key file until the
cutover is verified, then destroy it under the customer's key-handling policy.
Never put key values in shell command arguments, deployment env files or logs.

## Metadata backup and restore

```sh
apostille-local-admin backup --config "$CONFIG_FILE" --out "$BACKUP_DIRECTORY"
apostille-local-admin restore --source "$BACKUP_DIRECTORY" \
  --sha256 "$TRUSTED_BACKUP_SHA256" --out "$NEW_CONFIG_FILE"
```

Backup writes only validated configuration metadata plus a checksum manifest. It
does not read or copy API key values, TLS keys, signing seeds, model weights,
runtime images, prompts, outputs, evidence artifacts or caches. Keep its printed
hash independently. Restore refuses overwrites, verifies both checksums/schema,
and validates all installed model bundles against the restored pins. Model
bundles must be imported at their recorded absolute paths first. Provision TLS
keys, signing seeds and client API keys separately under the customer's secret
backup policy; referenced secret paths are metadata, not proof the secret exists.
Reapply the deployment permissions and run readiness and auth checks. Restoring
old metadata can reactivate old API key hashes: reconcile revocations before
starting the gateway.

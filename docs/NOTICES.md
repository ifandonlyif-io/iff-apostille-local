# Third-party dependency inventory

IFF's original code is MIT licensed. Third-party components retain their own
licenses and notices. This inventory was checked against `go.mod`, downloaded
module license files and the metadata of the SDK's clean test installation on
2026-10-08. It is **not a complete runtime-image SBOM or a redistribution bundle
of all required license texts**.

## Go dependencies

| Component | Selected version | License / upstream license |
| --- | --- | --- |
| Apostille Core | `v0.1.0-alpha.1` | [MIT](https://github.com/ifandonlyif-io/iff-apostille/blob/v0.1.0-alpha.1/LICENSE), copyright 2024 IfAndOnlyIf.io |
| santhosh-tekuri/jsonschema | `v6.0.2` | [Apache-2.0](https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.2/LICENSE) |
| cyberphone/json-canonicalization | `19d51d7fe467` (2024-12-13 pseudo-version) | [Apache-2.0](https://github.com/cyberphone/json-canonicalization/blob/19d51d7fe467/LICENSE), copyright 2018 Anders Rundgren |
| golang.org/x/text | `v0.41.0` | [BSD-3-Clause](https://cs.opensource.google/go/x/text/+/refs/tags/v0.41.0:LICENSE), copyright The Go Authors |
| Go standard library / selected toolchain | `go1.26.9` | [Go license](https://go.dev/LICENSE), BSD-3-Clause; included vendored components may have additional notices |

`go.mod` and `go.sum` are authoritative for the actual dependency versions and
module checksums. The license inventory must be refreshed when they change.

## Python SDK runtime dependencies

The package declares `httpx>=0.28.1,<0.29`. The following versions match the
Linux x86_64 / Python 3.11 preview dependency lock at
[`requirements-linux-py311.lock`](../sdk/python/requirements-linux-py311.lock),
and the observed Python 3.14.4 wheel-install test environment. The lock's hashes
and bundled wheels are authoritative for that preview; ordinary range-based
installs can resolve differently in the future.

| Component | Selected version | License / upstream source |
| --- | --- | --- |
| httpx | `0.28.1` | [BSD-3-Clause](https://github.com/encode/httpx/blob/0.28.1/LICENSE.md) |
| httpcore | `1.0.9` | [BSD-3-Clause](https://github.com/encode/httpcore/blob/1.0.9/LICENSE.md) |
| anyio | `4.15.1` | [MIT](https://github.com/agronholm/anyio/blob/master/LICENSE) |
| h11 | `0.16.0` | [MIT](https://github.com/python-hyper/h11/blob/v0.16.0/LICENSE.txt) |
| idna | `3.20` | [BSD-3-Clause](https://github.com/kjd/idna/blob/master/LICENSE.md) |
| certifi | `2026.7.22` | [MPL-2.0](https://github.com/certifi/python-certifi/blob/master/LICENSE) |
| typing_extensions | `4.16.0` | [PSF-2.0](https://github.com/python/typing_extensions/blob/main/LICENSE) |

Copy the actual installed distributions' license files into the customer release
notices bundle. In particular, certifi's certificate data and MPL obligations are
separate from this repository's MIT license. Keep the selected certificate bundle
and trust policy under the customer's update process.

Test-only customer clients (`tests/requirements-interop.txt`) are never gateway
or SDK dependencies. The Anthropic client resolves to `httpx2`/`httpcore2`
(separate distributions from the SDK's `httpx`), checked against the metadata of
the 2026-10-09 clean test installation:

| Component | Selected version | License / upstream source |
| --- | --- | --- |
| anthropic | `1.8.0` | [MIT](https://github.com/anthropics/anthropic-sdk-python/blob/v1.8.0/LICENSE) |
| httpx2 | `2.13.1` | BSD-3-Clause (package metadata) |
| httpcore2 | `2.13.1` | BSD-3-Clause (package metadata) |

Build/test utilities (for example setuptools, build, OpenAPI validation tools and
their dependencies) are not part of the SDK's declared runtime dependency list.
Include them in a build-environment SBOM if distributing that environment.

## Runtime and model exclusions

### Optional Flower integration

The source-only CPU demonstration directly pins `flwr==1.39.0` (Apache-2.0,
[upstream](https://github.com/flwrlabs/flower)) and `numpy==2.2.6` (BSD-3-Clause,
[upstream](https://github.com/numpy/numpy)) in
[`tests/requirements-flower.txt`](../tests/requirements-flower.txt). These are
optional integration/test dependencies, not native SDK or Go gateway runtime
dependencies. No upstream framework source is vendored here. The requirement
file pins direct dependencies; it is not a complete transitive hash lock.
Inventory all resolved packages and preserve their actual license texts before
redistributing a Flower environment. That environment is excluded from the
native SDK preview wheelhouse and its SPDX inventory.

### Inference deployment

No inference container, GPU driver, model weight, tokenizer, remote-code plugin or
vendor management package is licensed by this repository's MIT license. The
customer deployment must inventory its exact image digest, OS packages, CUDA or
ROCm components, inference engine and model artifacts. A vendor's broad
“open-source” description does not replace per-component licensing.

The runtime/model inventory cannot be completed before those artifacts are
selected. Model commercial use, redistribution and attribution must be checked
for each chosen revision. Preserve all applicable LICENSE/NOTICE files and any
source-delivery obligations in the actual release. See [RELEASE.md](RELEASE.md).

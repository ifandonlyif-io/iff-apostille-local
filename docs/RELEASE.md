# Preview release and supply-chain checklist

No registry publication, signing-key creation, hardware purchase or production
deployment is implied by building this repository.

## Software gates

From a clean, reviewed checkout:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install -e ./sdk/python build packaging
make PYTHON=.venv/bin/python check
make fuzz
make PYTHON=.venv/bin/python preview
```

`make check` covers Go tests, race detection, vet, Python unittest, the real
gateway/SDK integration test and host command builds. `make fuzz` exercises the
gateway JSON parser. `make preview` builds Linux amd64 binaries, Python wheel/sdist
and a local bundle under `dist/<VERSION>/`. It runs no Docker/GPU qualification
and performs no registry publication or customer rollout.

The preview generator writes `SHA256SUMS`, `PREVIEW.json`, resolved Python runtime
versions and `software.spdx.json`, with collected third-party license files. The
SPDX inventory covers Go modules/standard library and the SDK's runtime dependencies
from the bundled wheels. It does not cover the inference image, GPU
driver, model artifacts or Python interpreter. It is **not a complete customer
deployment SBOM**. Review the notice files against [NOTICES.md](NOTICES.md).

Install the generated wheel into a fresh virtual environment and exercise its
imports and mock suite. Validate the OpenAPI file and deployment configuration.
Record the actual tool versions and results; never convert a skipped GPU test
into a successful hardware claim.

`httpx>=0.28.1,<0.29` is the SDK's supported dependency range. The preview includes
`sdk/python/requirements-linux-py311.lock` with exact dependency versions and
hashes, dependency wheels in `wheelhouse/`, and the SDK wheel/sdist in `python/`.
The lock targets Linux x86_64 / Python 3.11; validate other combinations before
claiming support. Follow the [SDK offline installation instructions](../sdk/python/README.md)
to install without an index after checking the approved bundle's checksums.
Preserve the resolved wheel set in each customer release. Likewise preserve
`go.mod` and `go.sum`, exact runtime
image digests, model revisions/manifests and backend artifacts. Core 0.1 remains
the exact dependency version selected in `go.mod`.

## Required release artifacts

- Source revision and release notes with known limitations and supported tuple.
- Gateway/operator binaries with SHA-256 checksums and target OS/architecture.
- Python wheel/sdist and exact resolved dependency hashes.
- Deployment/configuration templates with no usable credentials or fake runtime
  digest represented as production-ready.
- SBOM for the Go binary, Python environment and actual runtime image, including
  transitive components; use SPDX or CycloneDX and record generator/version.
- Third-party license/NOTICE inventory for shipped software and each model.
- Vulnerability scan results with dates, databases/tool versions and explicit
  disposition of findings. A clean scan is not a blanket security certification.
- Synthetic software test results and the separate P4 hardware acceptance record.
- Installation, offline artifact transfer, backup, restore, rotation, rollback,
  support contacts and maintenance responsibilities.

Do not fabricate an SBOM from only direct dependencies, or mark it complete before
the actual image/model/dependency set exists. Do not include customer tokens,
private signing/TLS keys, prompts, completions or proprietary model weights in a
public artifact. Model redistribution requires the model's own permission.

## Claim review

Use “software preview” until the stated support matrix is actually verified.
Describe receipts as producer-signed metadata assertions. Do not claim TEE,
attested execution, output-content integrity, quantum resistance, non-exfiltration,
clinical correctness or pharmaceutical regulatory approval.

Publishing and customer rollout require an explicit release decision. Hardware
and runtime support commitments require the exact tested tuple, documented
service boundaries and a maintenance plan.

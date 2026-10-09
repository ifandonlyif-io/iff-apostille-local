# Apostille Local

This is an experimental self-hosted inference gateway, not a validated pharmaceutical system.
Never persist or log prompts, completions, authentication secrets, request bodies, or backend error bodies.
No cloud fallback, automatic downloads during inference, remote model code, public management API, or Docker socket in the gateway.
Core 0.1 and Core 0.3 are pinned dependencies; preserve their wire semantics. New keys are ML-DSA-65 (Core 0.3). Only Core 0.3 signatures are post-quantum; Core 0.1 (Ed25519, including the legacy raw seed) is classical. Producer-only signatures prove a signed metadata assertion, not output integrity, actual model execution, truth, or non-exfiltration.
Hardware support is unverified until the exact GPU/driver/image/model configuration passes real acceptance tests.
Use synthetic data and temporary directories in tests. No production credentials or model weights in Git.
Run go test ./..., go test -race ./..., go vet ./..., and the Python SDK unittest suite after relevant edits.

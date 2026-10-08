#!/usr/bin/env python3
"""Fail closed on local asset/config drift, then exec a qualified vLLM image.

No model downloads, shell, remote model code, pickle fallback or payload logs.
The image/driver/GPU combination still requires real hardware acceptance.
"""
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import sys

ALLOWED = {".json", ".safetensors", ".model", ".txt", ".md", ".tiktoken", ".vocab", ".merges"}
ATTRIBUTION = {"LICENSE", "LICENCE", "NOTICE", "COPYING"}


def fail(code):
    raise ValueError(code)


def no_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            fail("duplicate_json_key")
        result[key] = value
    return result


def read_json(path, limit):
    if path.is_symlink() or not path.is_file():
        fail("regular_file_required")
    with path.open("rb") as stream:
        raw = stream.read(limit + 1)
    if len(raw) > limit:
        fail("metadata_too_large")
    return json.loads(raw, object_pairs_hook=no_duplicate_keys), raw


def validate(config_path, bundle):
    config, _ = read_json(Path(config_path), 1 << 20)
    models = [m for m in config["models"] if m["id"] == config["active_model"]]
    if len(models) != 1:
        fail("active_model_missing")
    model = models[0]
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,95}", model["id"]):
        fail("invalid_model_id")
    if not re.fullmatch(r"[^\s]+@sha256:[a-f0-9]{64}", model["runtime_image"]):
        fail("image_digest_required")
    if model["precision"] not in ("float16", "bfloat16"):
        fail("unsupported_precision")
    if model.get("tool_call_parser", "") not in ("", "hermes"):
        fail("unsupported_tool_call_parser")
    if model.get("runtime_profile", "") not in ("", "vllm-chat-v1"):
        fail("unsupported_runtime_profile")
    for key, lower, upper in (("max_context", 128, 131072), ("max_tokens", 1, 131071), ("max_concurrent", 1, 32)):
        if type(model[key]) is not int or not lower <= model[key] <= upper:
            fail("invalid_model_limits")
    if model["max_tokens"] >= model["max_context"]:
        fail("invalid_model_limits")
    root = Path(bundle)
    if root.is_symlink() or not root.is_dir():
        fail("invalid_bundle_directory")
    manifest, raw = read_json(root / "manifest.json", 4 << 20)
    if hashlib.sha256(raw).hexdigest() != model["manifest_sha256"]:
        fail("manifest_hash_mismatch")
    expected_model = dict(model, path="", manifest_sha256="")
    if manifest.get("version") != 1 or manifest.get("model") != expected_model:
        fail("model_metadata_mismatch")
    entries = manifest.get("files")
    if not isinstance(entries, list) or not 1 <= len(entries) <= 100001:
        fail("invalid_manifest_files")
    seen = {"manifest.json"}
    has_weights = False
    for entry in entries:
        name = entry["path"]
        p = PurePosixPath(name)
        if (not name or p.is_absolute() or str(p) != name or ".." in p.parts or
                any(c in name for c in "\\\r\n\x00") or name in seen):
            fail("unsafe_asset_path")
        seen.add(name)
        if name != "image/runtime.tar":
            if (not name.startswith("model/") or (p.suffix.lower() not in ALLOWED and p.name.upper() not in ATTRIBUTION) or
                    any(part.startswith(".") for part in p.parts)):
                fail("unsupported_model_file")
            has_weights = has_weights or p.suffix.lower() == ".safetensors"
        path = root
        for part in p.parts:
            path = path / part
            if path.is_symlink():
                fail("symlinks_not_allowed")
        if not path.is_file() or type(entry["size"]) is not int or entry["size"] < 0:
            fail("regular_file_required")
        digest = hashlib.sha256()
        size = 0
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1 << 20), b""):
                size += len(chunk)
                digest.update(chunk)
        if size != entry["size"] or digest.hexdigest() != entry["sha256"]:
            fail("asset_hash_mismatch")
    if not has_weights or "model/config.json" not in seen or "image/runtime.tar" not in seen:
        fail("incomplete_bundle")
    for path in root.rglob("*"):
        if path.is_symlink() or (not path.is_dir() and path.relative_to(root).as_posix() not in seen):
            fail("unmanifested_or_linked_asset")
    model_config, _ = read_json(root / "model/config.json", 4 << 20)
    if not isinstance(model_config, dict) or "auto_map" in model_config:
        fail("remote_model_code_not_supported")
    return model


def command(model, bundle):
    if model.get("runtime_profile", "") not in ("", "vllm-chat-v1"):
        fail("unsupported_runtime_profile")
    args = [sys.executable, "-m", "vllm.entrypoints.openai.api_server",
            "--model", str(Path(bundle) / "model"), "--tokenizer", str(Path(bundle) / "model"),
            "--served-model-name", model["id"], "--host", "0.0.0.0", "--port", "8000",
            "--load-format", "safetensors", "--dtype", model["precision"],
            "--max-model-len", str(model["max_context"]), "--max-num-seqs", str(model["max_concurrent"]),
            "--default-chat-template-kwargs", '{"enable_thinking":false}',
            "--no-enable-log-requests", "--no-enable-log-outputs", "--disable-log-stats",
            "--disable-uvicorn-access-log"]
    parser = model.get("tool_call_parser", "")
    if parser not in ("", "hermes"):
        fail("unsupported_tool_call_parser")
    if parser:
        args += ["--enable-auto-tool-choice", "--tool-call-parser", parser]
    return args


def main():
    if len(sys.argv) != 3:
        fail("config_and_bundle_required")
    model = validate(sys.argv[1], sys.argv[2])
    # No inherited staging credentials or externally configured proxy/tracing sink.
    for key in tuple(os.environ):
        if (key.upper().endswith("_PROXY") or key.startswith(("HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "NGC_", "AWS_", "OTEL_", "WANDB_"))):
            os.environ.pop(key, None)
    os.environ.update({"HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "HF_DATASETS_OFFLINE": "1",
                       "HF_HUB_DISABLE_TELEMETRY": "1", "VLLM_NO_USAGE_STATS": "1", "DO_NOT_TRACK": "1",
                       "VLLM_LOGGING_LEVEL": "CRITICAL", "HF_HOME": "/tmp/huggingface",
                       "XDG_CACHE_HOME": "/tmp/cache", "TRITON_CACHE_DIR": "/tmp/triton",
                       "TORCHINDUCTOR_CACHE_DIR": "/tmp/torchinductor", "CUDA_CACHE_PATH": "/tmp/cuda"})
    # Runtime exception traces can include request text. Discard both streams;
    # the gateway reports bounded health/error codes without backend body content.
    with open(os.devnull, "wb") as sink:
        os.dup2(sink.fileno(), 1)
        os.dup2(sink.fileno(), 2)
    args = command(model, sys.argv[2])
    os.execv(args[0], args)


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("runtime_startup_validation_failed", file=sys.stderr)
        raise SystemExit(1)

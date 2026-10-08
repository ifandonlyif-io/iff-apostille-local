"""Synthetic SDK-to-Go integration, invoked by integration_test.go over verified TLS."""
import asyncio
import json
import os
from pathlib import Path
import subprocess
import sys

from apostille_local import APIError, AsyncClient, Client

url, ca_file, token_a, token_b, verifier, producer_pin, exports = sys.argv[1:]
exports = Path(exports)
options = dict(base_url=url, ca_file=ca_file, token_file=token_a)
messages = [{"role": "user", "content": "SYNTHETIC_PRIVATE_INPUT"}]


def verify_download(files, run_id, *, negative_checks=False):
    artifact, bundle = files["manifest"], files["bundle"]
    assert isinstance(artifact, Path) and isinstance(bundle, Path)
    assert artifact.stat().st_mode & 0o777 == 0o600
    assert bundle.stat().st_mode & 0o777 == 0o600
    assert artifact.parent.stat().st_mode & 0o777 == 0o700

    def offline(path, pin):
        return subprocess.run(
            [verifier, "--artifact", str(path), "--bundle", str(bundle), "--producer-pin", pin],
            capture_output=True, text=True, timeout=10, check=False,
        )

    result = offline(artifact, producer_pin)
    assert result.returncode == 0, "downloaded original failed offline verification"
    verified = json.loads(result.stdout)
    assert verified["valid"] and verified["signature_valid"] and verified["artifact_matches"]
    assert verified["producer_key_policy"] == "matched"
    assert verified["certificate_scope"] == "producer_only" and verified["issuer_trust"] == "unknown"
    assert verified["evidence_scope"] == "run_metadata_only" and verified["run_id"] == run_id
    if negative_checks:
        # Change one byte while preserving valid JSON and manifest field syntax;
        # this must fail because the artifact hash no longer matches the signature.
        original = artifact.read_bytes()
        changed = bytearray(original)
        index = original.index(b"qwen3-4b") + len(b"qwen3-4b") - 1
        changed[index] = ord("c")
        assert sum(a != b for a, b in zip(original, changed)) == 1
        assert json.loads(changed)["configured_model"]["id"] == "qwen3-4c"
        tampered = artifact.parent / "tampered-manifest.json"
        with os.fdopen(os.open(tampered, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as stream:
            stream.write(changed)
        rejected = offline(tampered, producer_pin)
        assert rejected.returncode != 0 and json.loads(rejected.stdout)["valid"] is False
        rejected = offline(artifact, "sha256:" + "0" * 64)
        assert rejected.returncode != 0 and json.loads(rejected.stdout)["valid"] is False


def check_receipt(client, result):
    assert result.run_id
    evidence = client.evidence.get(result.run_id)
    assert evidence["receipt_status"] == "ready"
    assert evidence["manifest"]["evidence_scope"] == "run_metadata_only"
    files = client.evidence.download(result.run_id, exports / ("sync-" + result.run_id))
    verify_download(files, result.run_id, negative_checks=True)
    return result.run_id


with Client(**options) as client:
    assert client.models.list()["data"][0]["id"] == "qwen3-4b"
    response = client.chat.create(model="qwen3-4b", messages=messages, record=True)
    assert response["choices"][0]["message"]["content"] == "SYNTHETIC_PRIVATE_OUTPUT"
    run_id = check_receipt(client, response)
    with Client(base_url=url, ca_file=ca_file, token_file=token_b) as other:
        assert other.models.list()["data"] == []
        try:
            other.evidence.get(run_id)
            raise AssertionError("cross-project evidence exposed")
        except APIError as error:
            assert error.status_code == 404
    schema = {"type": "json_schema", "json_schema": {"name": "answer", "strict": True,
              "schema": {"type": "object", "properties": {"answer": {"type": "integer"}},
                         "required": ["answer"], "additionalProperties": False}}}
    response = client.chat.create(model="qwen3-4b", messages=messages, response_format=schema)
    assert json.loads(response["choices"][0]["message"]["content"]) == {"answer": 42}
    with client.chat.stream(model="qwen3-4b", messages=messages, record=True) as stream:
        chunks = list(stream)
        assert chunks[-1]["choices"][0]["finish_reason"] == "stop"
        assert client.evidence.get(stream.run_id)["receipt_status"] == "ready"


async def main():
    async with AsyncClient(**options) as client:
        assert (await client.models.list())["data"][0]["id"] == "qwen3-4b"
        response = await client.chat.create(model="qwen3-4b", messages=messages, record=True)
        assert (await client.evidence.get(response.run_id))["receipt_status"] == "ready"
        files = await client.evidence.download(response.run_id, exports / ("async-" + response.run_id))
        verify_download(files, response.run_id)
        async with client.chat.stream(model="qwen3-4b", messages=messages, record=True) as stream:
            assert len([chunk async for chunk in stream]) == 2
            assert (await client.evidence.get(stream.run_id))["receipt_status"] == "ready"
        async with client.chat.stream(model="qwen3-4b", messages=[{"role": "user", "content": "cancel"}], record=True) as stream:
            async for _ in stream:
                break
            canceled = stream.run_id
        for _ in range(100):
            status = (await client.evidence.get(canceled))["receipt_status"]
            if status == "failed":
                break
            await asyncio.sleep(.02)
        assert status == "failed", "cancelled run acquired a success receipt"


asyncio.run(main())
print("SDK TLS integration passed: sync, async, SSE, cancellation, schema, evidence export, offline verification, tamper and pin rejection, isolation")

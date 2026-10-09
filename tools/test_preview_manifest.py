"""Synthetic release packaging tests; no compilation, download or publication."""
from contextlib import redirect_stdout
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

import preview_manifest as preview


class PreviewTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.version, self.sdk_version = preview.versions()
        sources = {
            "sdk/python/pyproject.toml": f'[project]\nversion = "{self.sdk_version}"\n',
            "internal/gateway/gateway.go": f'const Version = "{self.version}"\n',
            "api/openapi.yaml": f'info:\n  version: {self.version}\n',
            "Makefile": (preview.ROOT / "Makefile").read_text(),
            "LICENSE": "synthetic license\n", "README.md": "synthetic readme\n",
            "go.mod": "module synthetic\n", "go.sum": "synthetic sum\n",
            "sdk/python/README.md": "synthetic SDK readme\n",
            "sdk/python/requirements-linux-py311.lock": "synthetic lock\n",
        }
        for name, text in sources.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)
        self.out = self.root / "dist" / self.version
        self.go_root = self.root / "synthetic-go"
        self.go_root.mkdir()
        (self.go_root / "LICENSE").write_text("synthetic Go license")

    def prepare(self):
        return preview.prepare(self.out, self.version, self.root)

    def wheel(self, version=None):
        version = version or self.sdk_version
        directory = self.out / "python"
        directory.mkdir()
        wheel = directory / f"apostille_local-{version}-py3-none-any.whl"
        with zipfile.ZipFile(wheel, "w") as archive:
            archive.writestr(f"apostille_local-{version}.dist-info/METADATA",
                             f"Metadata-Version: 2.4\nName: apostille-local\nVersion: {version}\n")
        return wheel

    def go_output(self, args, **kwargs):
        if args == ["go", "env", "GOROOT"]:
            return str(self.go_root)
        if args == ["go", "env", "GOVERSION"]:
            return "go1.27.2"
        if args == ["go", "version"]:
            return "go version go1.27.2 linux/amd64"
        self.fail("unexpected external command")

    def finalize(self):
        modules = [
            {"Module": {"Path": "github.com/ifandonlyif-io/iff-apostille-local", "Main": True}},
            {"Module": {"Path": "github.com/ifandonlyif-io/iff-apostille", "Version": "v0.4.0-alpha.1"}},
        ]
        with patch.object(preview.subprocess, "run",
                          return_value=SimpleNamespace(stdout="\n".join(map(json.dumps, modules)))), \
                patch.object(preview.subprocess, "check_output", side_effect=self.go_output), \
                redirect_stdout(io.StringIO()):
            preview.finalize(self.out, self.version, self.root)

    def test_current_gateway_sdk_api_and_make_versions_are_coherent(self):
        self.assertEqual(preview.versions(), ("0.1.0-alpha.2", "0.1.0a2"))
        go_mod = (preview.ROOT / "go.mod").read_text()
        self.assertIn("github.com/ifandonlyif-io/iff-apostille v0.4.0-alpha.1", go_mod)

    def test_each_mismatched_source_rejects_before_output_creation(self):
        for name in ("internal/gateway/gateway.go", "api/openapi.yaml", "Makefile"):
            with self.subTest(name=name):
                path = self.root / name
                original = path.read_text()
                path.write_text(original.replace(self.version, "0.1.0-alpha.1"))
                with self.assertRaisesRegex(SystemExit, "local_release_version_mismatch"):
                    self.prepare()
                self.assertFalse(self.out.exists())
                path.write_text(original)
        with self.assertRaisesRegex(SystemExit, "requested_release_version_mismatch"):
            preview.prepare(self.out, "0.1.0-alpha.1", self.root)
        self.assertFalse(self.out.exists())

    def test_existing_output_is_never_reused(self):
        self.out.mkdir(parents=True)
        sentinel = self.out / "published.bin"
        sentinel.write_bytes(b"published synthetic bytes")
        with self.assertRaisesRegex(SystemExit, "preview_output_already_exists"):
            self.prepare()
        with self.assertRaisesRegex(SystemExit, "preview_output_not_fresh_reservation"):
            preview.finalize(self.out, self.version, self.root)
        self.assertEqual({path.name: path.read_bytes() for path in self.out.iterdir()},
                         {"published.bin": b"published synthetic bytes"})

    def test_make_guard_runs_before_any_builder(self):
        tools = self.root / "tools"
        tools.mkdir()
        shutil.copyfile(preview.ROOT / "tools/preview_manifest.py", tools / "preview_manifest.py")
        self.out.mkdir(parents=True)
        old_file = self.out / "old.bin"
        old_file.write_bytes(b"original")
        fake_bin = self.root / "fake-bin"
        fake_bin.mkdir()
        fake_go = fake_bin / "go"
        fake_go.write_text("#!/bin/sh\nprintf 'BUILDER_WAS_RUN\\n'\nexit 97\n")
        fake_go.chmod(0o700)
        result = subprocess.run(["make", "preview", f"PYTHON={sys.executable}"],
                                cwd=self.root, capture_output=True, text=True, timeout=15,
                                env={**os.environ, "PATH": str(fake_bin) + os.pathsep + os.defpath})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preview_output_already_exists", result.stderr)
        self.assertNotIn("BUILDER_WAS_RUN", result.stdout + result.stderr)
        self.assertEqual(old_file.read_bytes(), b"original")
        self.assertFalse((self.out / "linux-amd64").exists())

    def test_sbom_and_manifest_identify_local_alpha2_and_unchanged_core(self):
        self.prepare()
        self.wheel()
        self.finalize()
        manifest = json.loads((self.out / "PREVIEW.json").read_text())
        self.assertEqual(manifest["software_version"], self.version)
        self.assertEqual(manifest["sdk_version"], self.sdk_version)
        packages = {item["name"]: item["versionInfo"] for item in
                    json.loads((self.out / "software.spdx.json").read_text())["packages"]}
        self.assertEqual(packages["github.com/ifandonlyif-io/iff-apostille-local"], self.version)
        self.assertEqual(packages["github.com/ifandonlyif-io/iff-apostille"], "v0.4.0-alpha.1")
        self.assertEqual(packages["apostille-local"], self.sdk_version)
        checksums = (self.out / "SHA256SUMS").read_text()
        self.assertNotIn(".preview-", checksums)
        before = {path.relative_to(self.out): path.read_bytes() for path in self.out.rglob("*") if path.is_file()}
        with self.assertRaisesRegex(SystemExit, "preview_output_not_fresh_reservation"):
            self.finalize()
        with self.assertRaisesRegex(SystemExit, "preview_output_already_exists"):
            self.prepare()
        self.assertEqual(before, {path.relative_to(self.out): path.read_bytes()
                                  for path in self.out.rglob("*") if path.is_file()})

    def test_old_sdk_wheel_cannot_be_labeled_alpha2(self):
        self.prepare()
        self.wheel("0.1.0a1")
        with self.assertRaisesRegex(SystemExit, "sdk_wheel_version_mismatch"):
            self.finalize()
        self.assertFalse((self.out / "PREVIEW.json").exists())
        self.assertFalse((self.out / "SHA256SUMS").exists())
        with self.assertRaisesRegex(SystemExit, "preview_output_not_fresh_reservation"):
            self.finalize()


if __name__ == "__main__":
    unittest.main()

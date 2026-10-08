import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from contextlib import redirect_stdout
import io
from unittest.mock import patch


def module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    obj = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(obj)
    return obj


launcher = module(Path(__file__).with_name("runtime_launcher.py"), "launcher")
policy = module(Path(__file__).parent.parent / "scripts/egress_policy.py", "policy")
probe = module(Path(__file__).parent.parent / "scripts/egress_probe.py", "probe")


class RuntimeBoundaryTests(unittest.TestCase):
    def fixture(self, root):
        root = Path(root)
        bundle = root / "bundle"
        (bundle / "model").mkdir(parents=True)
        (bundle / "image").mkdir()
        model = dict(id="synthetic", revision="a" * 40, manifest_sha256="", license="synthetic-test-only",
                     runtime_image="registry.invalid/runtime@sha256:" + "b" * 64, precision="bfloat16",
                     max_context=4096, max_tokens=32, max_concurrent=1, path="")
        entries = []
        for name, raw in (("model/config.json", b'{}'), ("model/test.safetensors", b'synthetic-not-real-weights'),
                          ("model/LICENSE", b'synthetic-attribution'), ("model/README.md", b'synthetic-model-card'),
                          ("image/runtime.tar", b'synthetic-not-real-image')):
            (bundle / name).write_bytes(raw)
            entries.append(dict(path=name, sha256=hashlib.sha256(raw).hexdigest(), size=len(raw)))
        manifest = dict(version=1, model=dict(model), runtime_image_id="sha256:" + "c" * 64, files=entries)
        raw = json.dumps(manifest).encode()
        (bundle / "manifest.json").write_bytes(raw)
        model["manifest_sha256"] = hashlib.sha256(raw).hexdigest()
        model["path"] = str(bundle)
        config = root / "config.json"
        config.write_text(json.dumps(dict(active_model="synthetic", models=[model])))
        return config, bundle, model

    def test_verified_assets_and_safe_launch_flags(self):
        with tempfile.TemporaryDirectory() as tmp:
            config, bundle, expected = self.fixture(tmp)
            model = launcher.validate(config, bundle)
            self.assertEqual(model, expected)
            args = launcher.command(model, bundle)
            self.assertNotIn("--trust-remote-code", args)
            self.assertEqual(args[args.index("--load-format") + 1], "safetensors")
            self.assertIn("--no-enable-log-requests", args)
            self.assertIn("--no-enable-log-outputs", args)
            self.assertEqual(json.loads(args[args.index("--default-chat-template-kwargs") + 1]), {"enable_thinking": False})
            self.assertEqual(args[args.index("--model") + 1], str(bundle / "model"))

    def test_rejects_tamper_links_and_unmanifested_files(self):
        for mutation in ("tamper", "symlink", "pickle"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as tmp:
                config, bundle, _ = self.fixture(tmp)
                if mutation == "tamper":
                    (bundle / "model/test.safetensors").write_bytes(b'changed')
                elif mutation == "symlink":
                    (bundle / "model/link.json").symlink_to(config)
                else:
                    (bundle / "model/weights.pkl").write_bytes(b'pickle')
                with self.assertRaises(ValueError):
                    launcher.validate(config, bundle)

    def test_rejects_traversal_even_with_matching_manifest_hash(self):
        with tempfile.TemporaryDirectory() as tmp:
            config, bundle, model = self.fixture(tmp)
            manifest = json.loads((bundle / "manifest.json").read_bytes())
            manifest["files"][0]["path"] = "../config.json"
            raw = json.dumps(manifest).encode()
            (bundle / "manifest.json").write_bytes(raw)
            model["manifest_sha256"] = hashlib.sha256(raw).hexdigest()
            config.write_text(json.dumps(dict(active_model="synthetic", models=[model])))
            with self.assertRaisesRegex(ValueError, "unsafe_asset_path"):
                launcher.validate(config, bundle)


class FirewallScopeTests(unittest.TestCase):
    def test_no_global_flush_and_both_host_and_forward_paths(self):
        rules = policy.commands("172.29.80.0/24", "172.29.81.0/24")
        self.assertTrue(all("-F" not in r and "-P" not in r for r in rules))
        for chain in ("DOCKER-USER", "INPUT", "FORWARD"):
            self.assertTrue(any("-I" in r and chain in r for r in rules))
        for rule in rules:
            if "-I" in rule:
                self.assertIn("-i", rule)
                self.assertIn(rule[rule.index("-i") + 1], policy.BRIDGES)

    def test_refuses_broad_overlapping_or_public_networks(self):
        for ingress, inference in (("0.0.0.0/0", "172.29.81.0/24"), ("172.29.80.0/24", "172.29.80.0/24"),
                                   ("8.8.8.0/24", "172.29.81.0/24"), ("127.0.0.0/24", "172.29.81.0/24")):
            with self.subTest(ingress=ingress), self.assertRaises(ValueError):
                policy.commands(ingress, inference)


class EgressProbeTests(unittest.TestCase):
    def invoke(self, args, control=None, proxy=None):
        with tempfile.TemporaryDirectory() as tmp:
            if control is not None:
                path = Path(tmp) / "control.json"
                path.write_text(json.dumps(control))
                args = args + ["--control-report", str(path)]
            output = io.StringIO()
            with patch.object(probe.sys, "argv", ["probe"] + args), \
                 patch.object(probe, "connected", return_value=False), \
                 patch.object(probe.socket, "getaddrinfo", side_effect=OSError("synthetic denial")), \
                 patch.object(probe.socket, "create_connection", side_effect=OSError("synthetic denial") if proxy is False else None), \
                 redirect_stdout(output):
                status = probe.main()
            return status, json.loads(output.getvalue())

    def test_missing_positive_control_is_inconclusive(self):
        status, report = self.invoke(["--expect", "deny"])
        self.assertNotEqual(status, 0)
        self.assertEqual(report["result"], "inconclusive_positive_control_required")

    def test_unavailable_ipv6_is_never_claimed_tested(self):
        control = dict(result="positive_control_available", ipv4_outbound=True, external_dns=True, ipv6_outbound=False)
        status, report = self.invoke(["--expect", "deny"], control)
        self.assertEqual(status, 0)
        self.assertEqual(report["ipv6_coverage"], "unverified_no_positive_control")
        self.assertEqual(report["proxy_coverage"], "unverified")

    def test_proxy_requires_its_own_positive_control(self):
        control = dict(result="positive_control_available", ipv4_outbound=True, external_dns=True, ipv6_outbound=True)
        status, report = self.invoke(["--expect", "deny", "--proxy-host", "proxy.invalid", "--proxy-port", "3128"], control, proxy=False)
        self.assertNotEqual(status, 0)
        self.assertEqual(report["result"], "inconclusive_positive_control_required")


if __name__ == "__main__":
    unittest.main()

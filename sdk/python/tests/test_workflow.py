"""Recorder boundary tests; fixture signer is intentionally not cryptography.

Mandatory real Go signature and policy tests live in ../workflow_tests.
"""

from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
import uuid
from unittest.mock import patch

from apostille_local.workflow import ReceiptOutcome, WorkflowRecorder


_FAKE = r'''
import json, os, pathlib, sys, time
arguments = sys.argv[1:]
command = arguments[0]
def option(name):
    return arguments[arguments.index(name) + 1]
mode_file = pathlib.Path(__file__).with_name("mode")
mode = mode_file.read_text() if mode_file.exists() else ""
if command == "sign" and mode == "timeout":
    time.sleep(2)
if command == "sign" and mode == "fail":
    print("SENSITIVE_RUNTIME_ERROR", file=sys.stderr)
    print("SENSITIVE_SIGNING_OUTPUT")
    sys.exit(1)
if command == "sign":
    event = json.loads(pathlib.Path(option("--event")).read_text())
    if mode == "alter":
        event["model_id"] = "10000000-0000-4000-8000-000000000000"
    if mode == "environment":
        if os.environ.get("IFF_SYNTHETIC_SECRET"):
            sys.exit(1)
    target = pathlib.Path(option("--out"))
    with target.open("x") as output:
        json.dump({"event": event, "bundle": {"test_verified": True}}, output)
    target.chmod(0o600)
    if mode == "write_then_fail":
        sys.exit(1)
    print(json.dumps({"status": "ready", "event_id": event["event_id"]}))
elif command == "verify":
    receipt = json.loads(pathlib.Path(option("--receipt")).read_text())
    print(json.dumps({"valid": receipt["bundle"].get("test_verified") is True}))
elif command == "verify-set":
    paths = list(pathlib.Path(option("--directory")).glob("*.json"))
    valid = all(json.loads(path.read_text())["bundle"].get("test_verified") is True for path in paths)
    policy = json.loads(pathlib.Path(option("--policy")).read_text())
    print(json.dumps({"valid": valid, "record_count": len(paths),
                      "project_id": policy["project_id"], "job_id": policy["job_id"]}))
else:
    sys.exit(2)
'''


class RecorderBoundaryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.cli = self.root / "signer"
        self.cli.write_text(f"#!{sys.executable}\n" + _FAKE)
        self.cli.chmod(0o700)
        self.key = self.root / "key"
        self.key.write_text("synthetic-key-not-read-by-python")
        self.key.chmod(0o600)
        self.policy = self.root / "policy.json"
        self.policy.write_text("{}")
        self.kwargs = dict(executable=self.cli, archive_directory=self.root / "receipts",
                           key_file=self.key, policy_path=self.policy,
                           agent_id=str(uuid.uuid4()), project_id=str(uuid.uuid4()),
                           job_id=str(uuid.uuid4()), configuration_id=str(uuid.uuid4()),
                           model_id=str(uuid.uuid4()), framework="generic", framework_version="1.0")
        self.policy.write_text(json.dumps({"project_id": self.kwargs["project_id"],
                                           "job_id": self.kwargs["job_id"]}))

    def recorder(self, **overrides):
        return WorkflowRecorder(**(self.kwargs | overrides))

    def mode(self, value):
        (self.root / "mode").write_text(value)

    def test_record_resume_and_private_metadata_only(self):
        recorder = self.recorder()
        approved = recorder.record("configuration_approved")
        self.assertEqual(approved.status, "ready")
        self.assertIsInstance(approved, ReceiptOutcome)
        self.assertEqual(stat.S_IMODE(approved.receipt_path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(recorder.archive_directory.stat().st_mode), 0o700)
        event = json.loads(approved.receipt_path.read_bytes())["event"]
        self.assertEqual(event["sequence"], "1")
        self.assertEqual(event["artifact_sha256"], "")
        self.assertEqual(event["evidence_scope"], "workflow_metadata_only")
        outcome = self.recorder().record("work_completed", round=1)
        self.assertEqual(outcome.status, "ready")
        self.assertEqual(json.loads(outcome.receipt_path.read_bytes())["event"]["sequence"], "2")
        archive = b"".join(path.read_bytes() for path in recorder.archive_directory.glob("*.json"))
        self.assertNotIn(b"synthetic-key-not-read-by-python", archive)
        self.assertNotIn(str(self.key).encode(), archive)

    def test_repeated_terminal_round_is_blocked_after_restart(self):
        self.assertEqual(self.recorder().record("work_failed", round=1).status, "ready")
        again = self.recorder().record("work_completed", round=1)
        self.assertEqual((again.status, again.error), ("failed", "round_already_recorded"))
        self.assertEqual(len(list(self.kwargs["archive_directory"].glob("*.json"))), 1)

    def test_new_configuration_keeps_sequence_but_starts_own_rounds(self):
        self.assertEqual(self.recorder().record("work_completed", round=1).status, "ready")
        outcome = self.recorder(configuration_id=str(uuid.uuid4())).record("work_completed", round=1)
        self.assertEqual(outcome.status, "ready")
        self.assertEqual(json.loads(outcome.receipt_path.read_bytes())["event"]["sequence"], "2")

    def test_latest_round_is_persistent_scoped_and_verified(self):
        recorder = self.recorder()
        self.assertEqual(recorder.latest_round, 0)
        outcome = recorder.record("work_completed", round=3)
        self.assertEqual(outcome.status, "ready")
        self.assertEqual(self.recorder().latest_round, 3)
        self.assertEqual(self.recorder(configuration_id=str(uuid.uuid4())).latest_round, 0)
        receipt = json.loads(outcome.receipt_path.read_bytes())
        receipt["bundle"]["test_verified"] = False
        outcome.receipt_path.write_text(json.dumps(receipt))
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            _ = recorder.latest_round

    def test_signer_error_and_partial_output_never_advance_sequence(self):
        recorder = self.recorder()
        for mode in ("fail", "write_then_fail"):
            self.mode(mode)
            failed = recorder.record("work_completed", round=1)
            self.assertEqual(failed.error, "signing_failed")
            self.assertIsNone(failed.receipt_path)
            self.assertEqual(list(recorder.archive_directory.glob("*.json")), [])
            self.assertNotIn("SENSITIVE", repr(failed))
            self.assertEqual(list(recorder.archive_directory.glob(".apostille-workflow-*")), [])
        self.mode("")
        outcome = recorder.record("work_completed", round=1)
        self.assertEqual(json.loads(outcome.receipt_path.read_bytes())["event"]["sequence"], "1")

    def test_staging_shares_archive_filesystem_and_interrupted_staging_fails_closed(self):
        recorder = self.recorder()
        with patch("apostille_local.workflow.tempfile.TemporaryDirectory",
                   wraps=tempfile.TemporaryDirectory) as temporary:
            outcome = recorder.record("configuration_approved")
        self.assertEqual(outcome.status, "ready")
        self.assertEqual(temporary.call_args.kwargs["dir"], recorder.archive_directory)
        self.assertEqual(list(recorder.archive_directory.glob(".apostille-workflow-*")), [])
        interrupted = recorder.archive_directory / ".apostille-workflow-interrupted"
        interrupted.mkdir(mode=0o700)
        (interrupted / "event.json").write_text("synthetic interrupted metadata")
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        self.assertEqual(recorder.record("work_completed", round=1).error, "archive_invalid")
        self.assertEqual(len(list(recorder.archive_directory.glob("*.json"))), 1)

    def test_timeout_is_bounded_and_reports_fixed_code(self):
        recorder = self.recorder(timeout=0.2)
        self.mode("timeout")
        outcome = recorder.record("work_completed", round=1)
        self.assertEqual(outcome.error, "signer_timeout")
        self.assertEqual(list(recorder.archive_directory.glob("*.json")), [])

    def test_child_does_not_inherit_environment_secrets(self):
        recorder = self.recorder()
        self.mode("environment")
        with patch.dict(os.environ, {"IFF_SYNTHETIC_SECRET": "synthetic"}):
            self.assertEqual(recorder.record("configuration_approved").status, "ready")

    def test_signed_different_event_is_not_archived(self):
        recorder = self.recorder()
        self.mode("alter")
        self.assertEqual(recorder.record("configuration_approved").error, "receipt_invalid")
        self.assertEqual(list(recorder.archive_directory.glob("*.json")), [])

    def test_corrupted_or_foreign_archive_refuses_resume(self):
        outcome = self.recorder().record("configuration_approved")
        original = json.loads(outcome.receipt_path.read_bytes())
        for field in ("project_id", "job_id", "agent_id"):
            altered = json.loads(json.dumps(original))
            altered["event"][field] = str(uuid.uuid4())
            outcome.receipt_path.write_text(json.dumps(altered))
            with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
                self.recorder()
        outcome.receipt_path.write_text("broken")
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()

    def test_external_tampering_between_calls_is_rejected(self):
        recorder = self.recorder()
        first = recorder.record("configuration_approved")
        receipt = json.loads(first.receipt_path.read_bytes())
        receipt["bundle"]["test_verified"] = False
        first.receipt_path.write_text(json.dumps(receipt))
        self.assertEqual(recorder.record("work_completed", round=1).error, "archive_invalid")

    def test_missing_middle_or_renamed_receipt_refuses_resume(self):
        recorder = self.recorder()
        first = recorder.record("configuration_approved")
        second = recorder.record("work_completed", round=1)
        first.receipt_path.unlink()
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        second.receipt_path.rename(second.receipt_path.with_name(f"{uuid.uuid4()}.json"))
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()

    def test_unknown_files_and_receipt_symlinks_are_rejected(self):
        recorder = self.recorder()
        strange = recorder.archive_directory / "debug.log"
        strange.write_text("synthetic")
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        strange.unlink()
        (recorder.archive_directory / f"{uuid.uuid4()}.json").symlink_to(self.policy)
        with self.assertRaisesRegex(ValueError, "^unsafe_file$"):
            self.recorder()

    def test_unsafe_directory_key_lock_permissions_and_links(self):
        directory = self.kwargs["archive_directory"]
        directory.mkdir(mode=0o755)
        directory.chmod(0o755)
        with self.assertRaisesRegex(ValueError, "^unsafe_archive$"):
            self.recorder()
        directory.rmdir()
        directory.symlink_to(self.root, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "^unsafe_archive$"):
            self.recorder()
        directory.unlink()
        self.key.chmod(0o644)
        with self.assertRaisesRegex(ValueError, "^unsafe_file$"):
            self.recorder()
        self.key.chmod(0o600)
        directory.mkdir(mode=0o700)
        (directory / ".lock").symlink_to(self.key)
        with self.assertRaises(ValueError):
            self.recorder()

    def test_archive_replacement_fails_closed(self):
        recorder = self.recorder()
        recorder.archive_directory.rename(self.root / "old")
        recorder.archive_directory.mkdir(mode=0o700)
        self.assertEqual(recorder.record("configuration_approved").error, "unsafe_archive")

    def test_invalid_inputs_do_not_reach_signer(self):
        recorder = self.recorder()
        for round_value in (None, 0, -1, True, "01", "+1", "1e1", 2**53, "synthetic-secret"):
            self.assertEqual(recorder.record("work_completed", round=round_value).error, "invalid_round")
        self.assertEqual(recorder.record("configuration_approved", round=1).error, "invalid_round")
        self.assertEqual(recorder.record("arbitrary-prompt").error, "invalid_event")
        self.assertEqual(recorder.record("work_completed", round=1, artifact_path=self.policy).error,
                         "artifact_not_allowed")
        for overrides in ({"executable": "relative"}, {"agent_id": "secret"},
                          {"framework": "free form text"}, {"framework": "Flower"},
                          {"framework": "flower.demo"}, {"framework_version": "secret"}):
            with self.assertRaisesRegex(ValueError, "^invalid_configuration$"):
                self.recorder(**overrides)

    def test_concurrent_recorders_use_one_sequence_and_round_guard(self):
        first, second = self.recorder(), self.recorder()
        with ThreadPoolExecutor(max_workers=2) as pool:
            outcomes = list(pool.map(lambda recorder: recorder.record("work_completed", round=1),
                                     (first, second)))
        self.assertEqual(sorted(outcome.status for outcome in outcomes), ["failed", "ready"])
        self.assertEqual(next(outcome for outcome in outcomes if outcome.status == "failed").error,
                         "round_already_recorded")

    def test_exclusive_publication_does_not_overwrite_existing_receipt(self):
        recorder = self.recorder()
        first = recorder.record("configuration_approved")
        before = first.receipt_path.read_bytes()
        with patch("apostille_local.workflow.uuid.uuid4", return_value=uuid.UUID(first.event_id)):
            second = recorder.record("model_released")
        self.assertEqual(second.status, "failed")
        self.assertEqual(first.receipt_path.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()

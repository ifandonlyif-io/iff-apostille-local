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
    pq = "--require-post-quantum" in arguments and mode != "ignore_flag"
    print(json.dumps({"valid": receipt["bundle"].get("test_verified") is True,
                      "core_protocol": "https://ifandonlyif.io/apostille/spec/0.3" if pq or mode == "pq" else "https://ifandonlyif.io/apostille/spec/0.1"}))
elif command == "verify-set":
    paths = list(pathlib.Path(option("--directory")).glob("*.json"))
    valid = all(json.loads(path.read_text())["bundle"].get("test_verified") is True for path in paths)
    policy = json.loads(pathlib.Path(option("--policy")).read_text())
    pq = "--require-post-quantum" in arguments and mode != "ignore_flag"
    versions = ["https://ifandonlyif.io/apostille/spec/0.3"] if pq or mode == "pq" else ["https://ifandonlyif.io/apostille/spec/0.1"]
    print(json.dumps({"valid": valid, "record_count": len(paths), "core_protocols": versions,
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

    def test_require_post_quantum_passes_flag_and_checks_reported_versions(self):
        calls = []
        real = WorkflowRecorder._command

        def spy(recorder, *arguments, failure):
            calls.append(arguments)
            return real(recorder, *arguments, failure=failure)

        with patch.object(WorkflowRecorder, "_command", spy):
            self.assertEqual(self.recorder().record("configuration_approved").status, "ready")
            default = [c for c in calls if c[0] in ("verify", "verify-set")]
            self.assertTrue(default and all("--require-post-quantum" not in c for c in default))
            calls.clear()
            strict = self.recorder(require_post_quantum=True)
            self.assertEqual(strict.record("work_completed", round=1).status, "ready")
            checked = [c for c in calls if c[0] in ("verify", "verify-set")]
            self.assertTrue(checked and all("--require-post-quantum" in c for c in checked))
        # A verifier that ignores the flag and reports Core 0.1 is not trusted.
        self.mode("ignore_flag")
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder(require_post_quantum=True)
        self.mode("")
        with self.assertRaisesRegex(ValueError, "^invalid_configuration$"):
            self.recorder(require_post_quantum="yes")

    def test_require_post_quantum_rejects_receipt_reported_as_core_01(self):
        recorder = self.recorder(require_post_quantum=True)
        self.mode("ignore_flag")
        outcome = recorder.record("configuration_approved")
        self.assertEqual((outcome.status, outcome.error), ("failed", "archive_invalid"))

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
        observed = []
        original = recorder._command
        def command(*arguments, **kwargs):
            if arguments[0] == "sign":
                observed.append(Path(arguments[arguments.index("--out") + 1]).parent.parent)
            return original(*arguments, **kwargs)
        with patch.object(recorder, "_command", side_effect=command):
            outcome = recorder.record("configuration_approved")
        self.assertEqual(outcome.status, "ready")
        self.assertEqual(observed, [recorder.archive_directory])
        self.assertEqual(list(recorder.archive_directory.glob(".apostille-workflow-*")), [])
        interrupted = recorder.archive_directory / ".apostille-workflow-interrupted"
        interrupted.mkdir(mode=0o700)
        (interrupted / "event.json").write_text("synthetic interrupted metadata")
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        self.assertEqual(recorder.record("work_completed", round=1).error, "archive_invalid")
        self.assertEqual(len(list(recorder.archive_directory.glob("*.json"))), 1)

    def test_durable_attempt_survives_signing_failure_and_restart(self):
        recorder = self.recorder()
        recorder.reserve_round(1)
        self.mode("fail")
        self.assertEqual(recorder.record("work_completed", round=1).status, "failed")
        self.assertEqual(recorder.latest_round, 0)
        self.mode("")
        resumed = self.recorder()
        with self.assertRaisesRegex(ValueError, "^round_rejected$"):
            resumed.reserve_round(1)
        resumed.reserve_round(2)
        self.assertEqual(resumed.record("work_completed", round=2).status, "ready")
        self.assertEqual(resumed.latest_round, 2)
        self.assertEqual(json.loads((resumed.archive_directory / ".attempts").read_text())["streams"][0]["round"], "2")
        self.assertEqual(len(list(resumed.archive_directory.glob("*.json"))), 1)

    def test_atomic_pending_attempt_recovers_conservatively(self):
        recorder = self.recorder()
        with patch("apostille_local.workflow.os.replace", side_effect=OSError("synthetic")):
            with self.assertRaisesRegex(ValueError, "^attempt_state_unavailable$"):
                recorder.reserve_round(1)
        self.assertTrue((recorder.archive_directory / ".attempts.pending").exists())
        resumed = self.recorder()
        self.assertFalse((resumed.archive_directory / ".attempts.pending").exists())
        with self.assertRaisesRegex(ValueError, "^round_rejected$"):
            resumed.reserve_round(1)
        resumed.reserve_round(2)

    def test_attempts_are_context_bound_bounded_and_private(self):
        recorder = self.recorder()
        recorder.reserve_round(1)
        marker = recorder.archive_directory / ".attempts"
        self.assertEqual(stat.S_IMODE(marker.stat().st_mode), 0o600)
        original = marker.read_bytes()
        for field in ("agent_id", "project_id", "job_id"):
            value = json.loads(original)
            value[field] = str(uuid.uuid4())
            marker.write_text(json.dumps(value))
            with self.assertRaisesRegex(ValueError, "^attempt_state_invalid$"):
                self.recorder()
        marker.write_bytes(original)
        other = self.recorder(configuration_id=str(uuid.uuid4()))
        other.reserve_round(1)
        with patch("apostille_local.workflow._MAX_ATTEMPT_STREAMS", 2):
            third = self.recorder(configuration_id=str(uuid.uuid4()))
            with self.assertRaisesRegex(ValueError, "^attempt_state_full$"):
                third.reserve_round(1)
        marker.write_bytes(b"x" * (64 * 1024 + 1))
        with self.assertRaisesRegex(ValueError, "^attempt_state_invalid$"):
            self.recorder()

    def test_concurrent_attempt_reservation_has_one_winner(self):
        first, second = self.recorder(), self.recorder()
        def attempt(recorder):
            try:
                recorder.reserve_round(1)
                return "reserved"
            except ValueError:
                return "rejected"
        with ThreadPoolExecutor(max_workers=2) as pool:
            self.assertEqual(sorted(pool.map(attempt, (first, second))), ["rejected", "reserved"])

    def test_published_cleanup_failure_is_ready_and_readable_until_recovered(self):
        for failure in ("receipt.json", "event.json", "directory"):
            with self.subTest(failure=failure):
                # Separate archive each time; same independently pinned context.
                recorder = self.recorder(archive_directory=self.root / ("cleanup-" + failure))
                original_unlink, original_rmdir = Path.unlink, Path.rmdir
                def unlink(path, *args, **kwargs):
                    if path.name == failure and path.parent.name.startswith(".apostille-workflow-"):
                        raise PermissionError("PRIVATE_CLEANUP_MARKER")
                    return original_unlink(path, *args, **kwargs)
                def rmdir(path):
                    if failure == "directory" and path.name.startswith(".apostille-workflow-"):
                        raise PermissionError("PRIVATE_CLEANUP_MARKER")
                    return original_rmdir(path)
                with patch.object(Path, "unlink", unlink), patch.object(Path, "rmdir", rmdir):
                    outcome = recorder.record("work_completed", round=1)
                    self.assertEqual((outcome.status, outcome.warning), ("ready", "cleanup_pending"))
                    self.assertIsNone(outcome.error)
                    self.assertTrue(outcome.receipt_path.exists())
                    self.assertNotIn("PRIVATE", repr(outcome))
                    resumed = self.recorder(archive_directory=recorder.archive_directory)
                    self.assertEqual(resumed.latest_round, 1)
                    self.assertEqual(resumed.record("work_completed", round=1).error, "round_already_recorded")
                recovered = self.recorder(archive_directory=recorder.archive_directory)
                self.assertEqual(recovered.latest_round, 1)
                self.assertEqual(outcome.receipt_path.stat().st_nlink, 1)
                self.assertEqual(list(recovered.archive_directory.glob(".apostille-workflow-*")), [])

    def test_cleanup_bound_stops_writes_without_blocking_published_receipt_reads(self):
        recorder = self.recorder()
        original = Path.unlink
        def unlink(path, *args, **kwargs):
            if path.name == "receipt.json" and path.parent.name.startswith(".apostille-workflow-"):
                raise PermissionError("synthetic cleanup failure")
            return original(path, *args, **kwargs)
        with patch.object(Path, "unlink", unlink), patch("apostille_local.workflow._MAX_RECOVERY_STAGES", 1):
            first = recorder.record("work_completed", round=1)
            self.assertEqual((first.status, first.warning), ("ready", "cleanup_pending"))
            second = recorder.record("work_completed", round=2)
            self.assertEqual((second.status, second.error), ("failed", "cleanup_required"))
            self.assertEqual(self.recorder().latest_round, 1)
            self.assertEqual(len(list(recorder.archive_directory.glob("*.json"))), 1)
        resumed = self.recorder()
        self.assertEqual(resumed.record("work_completed", round=2).status, "ready")

    def test_named_staging_recovery_requires_matching_event_and_verified_publication(self):
        recorder = self.recorder()
        first = recorder.record("work_completed", round=1)
        original_receipt = first.receipt_path.read_bytes()
        original_event = json.loads(original_receipt)["event"]
        pending = recorder.archive_directory / f".apostille-workflow-{first.event_id}"
        pending.mkdir(mode=0o700)
        staged_event = pending / "event.json"
        altered = original_event | {"model_id": str(uuid.uuid4())}
        staged_event.write_text(json.dumps(altered))
        staged_event.chmod(0o600)
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        self.assertTrue(staged_event.exists())
        self.assertEqual(first.receipt_path.read_bytes(), original_receipt)
        # Matching metadata alone is insufficient when independent verification
        # rejects the purported committed receipt.
        staged_event.write_text(json.dumps(original_event))
        invalid = json.loads(original_receipt)
        invalid["bundle"]["test_verified"] = False
        first.receipt_path.write_text(json.dumps(invalid))
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        self.assertTrue(staged_event.exists())
        first.receipt_path.write_bytes(original_receipt)
        self.assertEqual(self.recorder().latest_round, 1)
        self.assertFalse(pending.exists())

    def test_postpublication_sync_failure_does_not_remove_committed_receipt(self):
        recorder = self.recorder()
        with patch.object(recorder, "_sync_directory", side_effect=OSError("PRIVATE_SYNC_MARKER")):
            outcome = recorder.record("work_completed", round=1)
        self.assertEqual((outcome.status, outcome.warning), ("ready", "durability_uncertain"))
        self.assertTrue(outcome.receipt_path.exists())
        self.assertEqual(self.recorder().latest_round, 1)
        self.assertNotIn("PRIVATE", repr(outcome))

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

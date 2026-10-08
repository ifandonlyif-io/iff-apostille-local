"""Mandatory offline integration tests against the actual Go signing CLI."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import uuid

from apostille_local.workflow import WorkflowRecorder


_EVENTS = ["configuration_approved", "work_completed", "work_failed", "work_cancelled",
           "model_released", "deployment_accepted"]


class WorkflowLiveTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        raw = os.environ.get("APOSTILLE_WORKFLOW_BIN", "")
        if not raw or not Path(raw).is_absolute() or not os.access(raw, os.X_OK):
            raise RuntimeError("build apostille-workflow and set APOSTILLE_WORKFLOW_BIN")
        cls.cli = Path(raw)

    def command(self, *arguments, valid=True):
        result = subprocess.run([str(self.cli), *map(str, arguments)],
                                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, timeout=30,
                                env={"LANG": "C", "LC_ALL": "C"})
        if valid:
            self.assertEqual(result.returncode, 0, "workflow CLI rejected synthetic fixture")
        else:
            self.assertNotEqual(result.returncode, 0, "workflow CLI accepted invalid evidence")
        if result.returncode:
            return None
        return json.loads(result.stdout)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.agent = str(uuid.uuid4())
        self.key = self.root / "producer.json"
        key_info = self.command("keygen", "--out-key", self.key, "--agent-id", self.agent)
        self.policy = self.root / "policy.json"
        self.policy_value = {"schema": "urn:apostille:workflow-policy:0.1",
                             "project_id": str(uuid.uuid4()), "job_id": str(uuid.uuid4()),
                             "producers": [{"agent_id": self.agent, "key_id": key_info["producer_pin"],
                                            "event_types": _EVENTS}]}
        self.write_policy()
        self.kwargs = dict(executable=self.cli, archive_directory=self.root / "receipts",
                           key_file=self.key, policy_path=self.policy, agent_id=self.agent,
                           project_id=self.policy_value["project_id"], job_id=self.policy_value["job_id"],
                           configuration_id=str(uuid.uuid4()), model_id=str(uuid.uuid4()),
                           framework="generic", framework_version="1.0")

    def write_policy(self):
        self.policy.write_text(json.dumps(self.policy_value))

    def recorder(self, **overrides):
        return WorkflowRecorder(**(self.kwargs | overrides))

    def test_true_signature_resume_and_persistent_round_guard(self):
        first = self.recorder().record("configuration_approved")
        self.assertEqual(first.status, "ready", first.error)
        second = self.recorder().record("work_completed", round=1)
        self.assertEqual(second.status, "ready", second.error)
        verified = self.command("verify-set", "--directory", self.kwargs["archive_directory"],
                                "--policy", self.policy)
        self.assertIs(verified["valid"], True)
        self.assertEqual(verified["record_count"], 2)
        self.assertEqual(verified["archive_completeness"], "unknown")
        self.assertEqual(self.recorder().record("work_cancelled", round=1).error,
                         "round_already_recorded")
        event = json.loads(second.receipt_path.read_bytes())["event"]
        self.assertEqual(event["sequence"], "2")
        self.assertEqual(event["agent_id"], self.agent)

    def test_out_of_order_round_rejected_before_archive_publication(self):
        recorder = self.recorder()
        for round_number in (1, 3):
            self.assertEqual(recorder.record("work_completed", round=round_number).status, "ready")
        before = {path.name: path.read_bytes() for path in recorder.archive_directory.glob("*.json")}
        self.assertEqual(recorder.record("work_completed", round=2).error, "round_not_increasing")
        after = {path.name: path.read_bytes() for path in recorder.archive_directory.glob("*.json")}
        self.assertEqual(before, after)
        self.assertEqual(self.recorder().record("work_completed", round=4).status, "ready")

    def test_empty_archive_rejects_wrong_policy_project_or_job(self):
        for field in ("project_id", "job_id"):
            original = self.policy_value[field]
            self.policy_value[field] = str(uuid.uuid4())
            self.write_policy()
            with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
                self.recorder()
            self.policy_value[field] = original
            self.write_policy()

    def test_wrong_independent_pin_and_changed_authorization_reject_resume(self):
        self.assertEqual(self.recorder().record("work_completed", round=1).status, "ready")
        correct = self.policy_value["producers"][0]["key_id"]
        self.policy_value["producers"][0]["key_id"] = "sha256:" + "0" * 64
        self.write_policy()
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()
        self.policy_value["producers"][0]["key_id"] = correct
        self.policy_value["producers"][0]["event_types"] = ["configuration_approved"]
        self.write_policy()
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()

    def test_tampered_event_is_rejected(self):
        recorder = self.recorder()
        first = recorder.record("configuration_approved")
        self.assertEqual(first.status, "ready", first.error)
        receipt = json.loads(first.receipt_path.read_bytes())
        receipt["event"]["model_id"] = str(uuid.uuid4())
        first.receipt_path.write_text(json.dumps(receipt))
        self.command("verify", "--receipt", first.receipt_path, "--policy", self.policy, valid=False)
        self.assertEqual(recorder.record("work_completed", round=1).error, "archive_invalid")

    def test_replay_under_another_filename_rejected(self):
        first = self.recorder().record("configuration_approved")
        self.assertEqual(first.status, "ready", first.error)
        duplicate = first.receipt_path.with_name(f"{uuid.uuid4()}.json")
        shutil.copyfile(first.receipt_path, duplicate)
        duplicate.chmod(0o600)
        self.command("verify-set", "--directory", self.kwargs["archive_directory"],
                     "--policy", self.policy, valid=False)
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()

    def test_key_failure_leaves_no_success_and_retry_does_not_repeat_work(self):
        recorder = self.recorder()
        good_key = self.key.read_bytes()
        self.key.write_text("invalid-synthetic-key")
        outcome = recorder.record("work_completed", round=1)
        self.assertEqual(outcome.error, "signing_failed")
        self.assertEqual(list(recorder.archive_directory.glob("*.json")), [])
        self.key.write_bytes(good_key)
        outcome = recorder.record("work_completed", round=1)
        self.assertEqual(outcome.status, "ready", outcome.error)
        self.assertEqual(json.loads(outcome.receipt_path.read_bytes())["event"]["sequence"], "1")
        data = outcome.receipt_path.read_bytes()
        self.assertNotIn(good_key, data)
        self.assertNotIn(str(self.key).encode(), data)

    def test_valid_wrong_key_cannot_publish_or_poison_archive(self):
        other_key = self.root / "wrong-key.json"
        self.command("keygen", "--out-key", other_key, "--agent-id", self.agent)
        recorder = self.recorder(key_file=other_key)
        rejected = recorder.record("work_completed", round=1)
        self.assertEqual(rejected.status, "failed")
        self.assertIn(rejected.error, {"receipt_invalid", "signing_failed"})
        self.assertEqual(list(recorder.archive_directory.glob("*.json")), [])
        accepted = self.recorder().record("work_completed", round=1)
        self.assertEqual(accepted.status, "ready", accepted.error)
        self.assertEqual(json.loads(accepted.receipt_path.read_bytes())["event"]["sequence"], "1")

    def test_release_optional_artifact_binding_and_tamper(self):
        artifact = self.root / "synthetic-model.bin"
        artifact.write_bytes(b"approved synthetic model bytes")
        outcome = self.recorder().record("model_released", artifact_path=artifact)
        self.assertEqual(outcome.status, "ready", outcome.error)
        event = json.loads(outcome.receipt_path.read_bytes())["event"]
        self.assertNotEqual(event["artifact_sha256"], "")
        self.assertEqual(event["artifact_size"], str(artifact.stat().st_size))
        self.assertNotIn(str(artifact), outcome.receipt_path.read_text())
        self.command("verify", "--receipt", outcome.receipt_path, "--policy", self.policy,
                     "--artifact", artifact)
        artifact.write_bytes(b"altered synthetic model bytes")
        self.command("verify", "--receipt", outcome.receipt_path, "--policy", self.policy,
                     "--artifact", artifact, valid=False)
        # Resumption needs only signed metadata, not a retained model copy.
        artifact.unlink()
        self.recorder()

    def test_signed_failure_and_cancellation_never_become_success(self):
        recorder = self.recorder()
        failed = recorder.record("work_failed", round=1)
        cancelled = recorder.record("work_cancelled", round=2)
        self.assertEqual((failed.status, cancelled.status), ("ready", "ready"))
        events = [json.loads(path.read_bytes())["event"]["event_type"]
                  for path in recorder.archive_directory.glob("*.json")]
        self.assertEqual(sorted(events), ["work_cancelled", "work_failed"])
        for index in (1, 2):
            self.assertEqual(self.recorder().record("work_completed", round=index).error,
                             "round_already_recorded")

    def test_shared_policy_does_not_allow_foreign_actor_in_dedicated_archive(self):
        first = self.recorder().record("configuration_approved")
        self.assertEqual(first.status, "ready", first.error)
        other_agent = str(uuid.uuid4())
        other_key = self.root / "other-key.json"
        key_info = self.command("keygen", "--out-key", other_key, "--agent-id", other_agent)
        self.policy_value["producers"].append({"agent_id": other_agent,
                                              "key_id": key_info["producer_pin"],
                                              "event_types": _EVENTS})
        self.write_policy()
        other = self.recorder(agent_id=other_agent, key_file=other_key,
                              archive_directory=self.root / "other-receipts")
        other_receipt = other.record("configuration_approved")
        self.assertEqual(other_receipt.status, "ready", other_receipt.error)
        target = self.kwargs["archive_directory"] / other_receipt.receipt_path.name
        shutil.copyfile(other_receipt.receipt_path, target)
        target.chmod(0o600)
        with self.assertRaisesRegex(ValueError, "^archive_invalid$"):
            self.recorder()


if __name__ == "__main__":
    unittest.main()

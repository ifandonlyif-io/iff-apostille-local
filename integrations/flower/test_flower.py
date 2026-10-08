"""Real Flower hook and CLI integration tests; dependencies are mandatory here."""
from adapter import EvidenceFedAvg, Participant, RoundRejected
from demo import InProcessNodes, cli_json, initialize_in_process_task, make_client, run_demo

import contextlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import uuid

import numpy as np
from flwr.app import ArrayRecord, ConfigRecord, Context, Error, Message, MetricRecord, RecordDict
from flwr.clientapp import ClientApp
from flwr.serverapp.strategy import FedAvg
from apostille_local.workflow import ReceiptOutcome, WorkflowRecorder


class RecorderSpy:
    """Only the signer boundary is stubbed in hook tests; Flower is always real."""
    def __init__(self, status="ready", raises=False):
        self.configuration_id = "approved"
        self.events = []
        self.status = status
        self.raises = raises

    @property
    def latest_round(self):
        return max((server_round for _, server_round in self.events), default=0) if self.status == "ready" else 0

    def record(self, event_type, *, round):
        self.events.append((event_type, round))
        if self.raises:
            raise RuntimeError("PRIVATE_TRAINING_MARKER")
        return ReceiptOutcome(self.status, None, str(uuid.uuid4()),
                              None if self.status == "ready" else "signing_failed")


def request(node=1, server_round=1, configuration_id="approved"):
    return Message(content=RecordDict({
        "arrays": ArrayRecord([np.zeros(3)]),
        "config": ConfigRecord({"server-round": server_round, "configuration-id": configuration_id}),
    }), dst_node_id=node, message_type="train", group_id=str(server_round))


def response(message, weight=1.0):
    return Message(content=RecordDict({
        "arrays": ArrayRecord([np.full(3, weight)]),
        "metrics": MetricRecord({"num-examples": 2}),
    }), reply_to=message)


def context(node=1):
    return Context(1, node, {}, RecordDict(), {})


class AdapterTests(unittest.TestCase):
    def setUp(self):
        initialize_in_process_task()

    def test_environment_is_disabled_before_flower_import(self):
        code = (
            "import adapter, os, sys; "
            "from flwr.supercore import telemetry; "
            "assert telemetry.FLWR_TELEMETRY_ENABLED == '0'; "
            "assert os.environ['FLWR_DISABLE_RUNTIME_DEPENDENCY_INSTALLATION'] == '1'; "
            "assert 'torch' not in sys.modules and 'ray' not in sys.modules"
        )
        result = subprocess.run([sys.executable, "-c", code], capture_output=True, timeout=30,
                                env={**os.environ, "FLWR_TELEMETRY_ENABLED": "1"})
        self.assertEqual(result.returncode, 0, result.stderr.decode())

    def test_training_happens_before_completed_receipt(self):
        spy = RecorderSpy()
        participant = Participant(spy, "approved", allow_in_process_messages=True)
        app = ClientApp()
        result = response(request())
        @app.train()
        @participant.wrap
        def train(message, context):
            self.assertEqual(spy.events, [])
            return result
        self.assertIs(app(request(), context()), result)
        self.assertEqual(spy.events, [("work_completed", 1)])

    def test_signing_failure_preserves_result_without_retry(self):
        for raises in (False, True):
            with self.subTest(raises=raises):
                spy = RecorderSpy("failed", raises)
                participant = Participant(spy, "approved", allow_in_process_messages=True)
                calls = []
                def train(message, context):
                    calls.append(1)
                    return response(message)
                callback = participant.wrap(train)
                self.assertFalse(callback(request(), context()).has_error())
                self.assertEqual(participant.last_receipt.error, "signing_failed")
                with self.assertRaises(RoundRejected):
                    callback(request(), context())
                self.assertEqual(calls, [1])

    def test_exception_and_interrupt_record_fixed_failed_or_cancelled(self):
        for exception, event in ((RuntimeError("PRIVATE_TRAINING_MARKER"), "work_failed"),
                                 (KeyboardInterrupt("PRIVATE_TRAINING_MARKER"), "work_cancelled")):
            with self.subTest(event=event):
                spy = RecorderSpy()
                participant = Participant(spy, "approved", allow_in_process_messages=True)
                def train(message, context):
                    raise exception
                out, err = io.StringIO(), io.StringIO()
                with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                    result = participant.wrap(train)(request(), context())
                self.assertTrue(result.has_error())
                self.assertNotIn("PRIVATE", result.error.reason + out.getvalue() + err.getvalue())
                self.assertEqual(spy.events, [(event, 1)])

    def test_stale_wrong_config_and_skipped_rounds_never_execute(self):
        spy = RecorderSpy()
        callback = Participant(spy, "approved", allow_in_process_messages=True).wrap(lambda *_: self.fail("must not train"))
        other_run = request()
        other_run.metadata._run_id = 99999
        for message in (request(server_round=2), request(configuration_id="other"), request(node=2), other_run):
            with self.assertRaises(RoundRejected):
                callback(message, context())
        self.assertEqual(spy.events, [])

    def test_wrong_round_success_reply_is_not_signed_completed(self):
        spy = RecorderSpy()
        stale = response(request(server_round=2))
        answer = Participant(spy, "approved", allow_in_process_messages=True).wrap(lambda *_: stale)(request(), context())
        self.assertTrue(answer.has_error())
        self.assertEqual(spy.events, [("work_failed", 1)])

    def test_configuration_mismatch_is_rejected_before_callbacks(self):
        spy = RecorderSpy()
        with self.assertRaisesRegex(ValueError, "configuration_mismatch"):
            Participant(spy, "different")
        with self.assertRaisesRegex(ValueError, "configuration_mismatch"):
            EvidenceFedAvg(spy, "different", (1, 2, 3))
        participant = Participant(spy, "approved", allow_in_process_messages=True)
        spy.configuration_id = "changed"
        with self.assertRaises(RoundRejected):
            participant.wrap(lambda *_: self.fail("must not train"))(request(), context())

    def test_unassigned_request_ids_require_explicit_simulation_opt_in(self):
        spy = RecorderSpy()
        with self.assertRaises(RoundRejected):
            Participant(spy, "approved").wrap(lambda *_: self.fail("must not train"))(request(), context())
        strategy = EvidenceFedAvg(spy, "approved", (1, 2, 3))
        requests = list(strategy.configure_train(1, ArrayRecord([np.zeros(3)]),
                                                ConfigRecord(), InProcessNodes()))
        self.assertEqual(strategy.aggregate_train(1, [response(m) for m in requests]), (None, None))
        self.assertEqual(spy.events, [("work_failed", 1)])

    def test_reply_correlation_fields_reject_other_work(self):
        for field, value in (("_run_id", 99999), ("_dst_node_id", 777),
                             ("_reply_to_message_id", "unrelated-request"),
                             ("_message_type", "evaluate"), ("_dst_task_id", 999)):
            with self.subTest(field=field):
                spy, strategy, replies = self.strategy()
                setattr(replies[0].metadata, field, value)
                self.assertEqual(strategy.aggregate_train(1, replies), (None, None))
                self.assertEqual(spy.events, [("work_failed", 1)])
                client_spy = RecorderSpy()
                wrong_reply = response(request())
                setattr(wrong_reply.metadata, field, value)
                answer = Participant(client_spy, "approved", allow_in_process_messages=True).wrap(lambda *_: wrong_reply)(request(), context())
                self.assertTrue(answer.has_error())
                self.assertEqual(client_spy.events, [("work_failed", 1)])

    def test_assigned_request_and_task_ids_must_match(self):
        for wrong in ("request", "task", None):
            with self.subTest(wrong=wrong):
                spy = RecorderSpy()
                strategy = EvidenceFedAvg(spy, "approved", (1, 2, 3), allow_in_process_messages=False)
                messages = list(strategy.configure_train(1, ArrayRecord([np.zeros(3)]),
                                                        ConfigRecord(), InProcessNodes()))
                # The transport assigns IDs after configure_train returns.
                for message in messages:
                    message.metadata._message_id = str(uuid.uuid4())
                    message.metadata.dst_task_id = message.metadata.dst_node_id + 500
                replies = [response(message) for message in messages]
                if wrong == "request":
                    replies[0].metadata._reply_to_message_id = "different-request"
                if wrong == "task":
                    replies[0].metadata.src_task_id = 999
                arrays, _ = strategy.aggregate_train(1, replies)
                self.assertEqual(arrays is None, wrong is not None)
                self.assertEqual(spy.events, [("work_completed" if wrong is None else "work_failed", 1)])

    def test_error_reply_and_nonfinite_result_are_failed(self):
        for result in (Message(error=Error(1, "PRIVATE_TRAINING_MARKER"), reply_to=request()),
                       response(request(), np.nan)):
            spy = RecorderSpy()
            callback = Participant(spy, "approved", allow_in_process_messages=True).wrap(lambda *_: result)
            answer = callback(request(), context())
            self.assertTrue(answer.has_error())
            self.assertEqual(answer.error.reason, "training_failed")
            self.assertEqual(spy.events, [("work_failed", 1)])

    def strategy(self, spy=None):
        spy = spy or RecorderSpy()
        strategy = EvidenceFedAvg(spy, "approved", (1, 2, 3), allow_in_process_messages=True)
        messages = list(strategy.configure_train(1, ArrayRecord([np.zeros(3)]),
                                                ConfigRecord(), InProcessNodes()))
        return spy, strategy, [response(m, float(m.metadata.dst_node_id)) for m in messages]

    def test_real_fedavg_aggregates_and_signature_failure_preserves_model(self):
        for status in ("ready", "failed"):
            spy, strategy, replies = self.strategy(RecorderSpy(status))
            with patch.object(FedAvg, "aggregate_train", autospec=True,
                              side_effect=FedAvg.aggregate_train) as actual:
                arrays, _ = strategy.aggregate_train(1, replies)
            self.assertEqual(actual.call_count, 1)
            np.testing.assert_allclose(arrays.to_numpy_ndarrays()[0], np.full(3, 2.0))
            self.assertEqual(spy.events, [("work_completed", 1)])
            self.assertEqual(strategy.last_receipt.status, status)
            with self.assertRaises(RoundRejected):
                strategy.aggregate_train(1, replies)

    def test_missing_duplicate_stale_and_failed_participants_prevent_success(self):
        for kind in ("missing", "duplicate", "stale", "failed"):
            with self.subTest(kind=kind):
                spy, strategy, replies = self.strategy()
                if kind == "missing":
                    replies.pop()
                elif kind == "duplicate":
                    replies[0] = replies[1]
                elif kind == "stale":
                    replies[0].metadata.group_id = "0"
                else:
                    replies[0] = Message(error=Error(1, "PRIVATE_TRAINING_MARKER"),
                                         reply_to=request(node=replies[0].metadata.src_node_id))
                with patch.object(FedAvg, "aggregate_train") as actual:
                    self.assertEqual(strategy.aggregate_train(1, replies), (None, None))
                actual.assert_not_called()
                self.assertEqual(spy.events, [("work_failed", 1)])

    def test_aggregation_exception_or_empty_model_has_no_success_receipt(self):
        for mode in ("exception", "empty"):
            spy, strategy, replies = self.strategy()
            options = ({"side_effect": RuntimeError("PRIVATE_TRAINING_MARKER")} if mode == "exception"
                       else {"return_value": (None, None)})
            with patch.object(FedAvg, "aggregate_train", **options):
                self.assertEqual(strategy.aggregate_train(1, replies), (None, None))
            self.assertEqual(spy.events, [("work_failed", 1)])

    def test_real_numpy_training_improves_model(self):
        participant = Participant(RecorderSpy(), "approved", allow_in_process_messages=True)
        app, state = make_client(participant, 1)
        weights = app(request(), state).content["arrays"].to_numpy_ndarrays()[0]
        target = np.array([1.5, -0.75, 0.2])
        self.assertLess(np.linalg.norm(weights - target), np.linalg.norm(target))


class EndToEndTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.executable = Path(os.environ["APOSTILLE_WORKFLOW_BIN"])
        if not cls.executable.is_absolute() or not cls.executable.is_file():
            raise RuntimeError("build apostille-workflow and set APOSTILLE_WORKFLOW_BIN")

    def test_both_industries_real_signed_offline_demo(self):
        for scenario in ("manufacturing", "pharma"):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "demo"
                # No outgoing connections even if a dependency accidentally tries
                # them. subprocess calls only the local offline Go executable.
                with patch("socket.socket.connect", side_effect=AssertionError("network_forbidden")), \
                     patch("socket.getaddrinfo", side_effect=AssertionError("dns_forbidden")):
                    summary = run_demo(self.executable, output, scenario, True)
                self.assertEqual(summary["receipt_count"], 15)
                self.assertEqual(summary["receipt_status"], "ready")
                policy = output / "receiver-policy.json"
                verified = cli_json(self.executable, "verify-set", "--directory", str(output / "export"),
                                    "--policy", str(policy))
                self.assertTrue(verified["valid"])
                for path in output.rglob("*"):
                    self.assertEqual(path.stat().st_mode & 0o077, 0)
                events = [json.loads(path.read_text())["event"] for path in (output / "export").glob("*.json")]
                self.assertEqual(sum(e["event_type"] == "work_completed" for e in events), 12)
                self.assertEqual({e["schema"] for e in events}, {"urn:apostille:workflow-event:0.1"})
                for event in events:
                    self.assertNotIn("scenario", event)
                    self.assertNotIn("metrics", event)
                    self.assertNotIn("arrays", event)
                    self.assertNotIn("gradient", event)
                # Full artifact verification rejects a changed released model.
                released = next(p for p in (output / "export").glob("*.json")
                                if json.loads(p.read_text())["event"]["event_type"] == "model_released")
                artifact = output / "synthetic-model.npy"
                artifact.write_bytes(b"tampered synthetic model")
                with self.assertRaises(RuntimeError):
                    cli_json(self.executable, "verify", "--receipt", str(released), "--policy", str(policy),
                             "--artifact", str(artifact))

    def test_restart_uses_verified_archive_before_executing_training(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "demo"
            run_demo(self.executable, output, "manufacturing")
            archive = output / "site-1" / "receipts"
            # These are this test's freshly generated, independently verified
            # identities. Production config must come from the administrator.
            event = json.loads(next(archive.glob("*.json")).read_text())["event"]
            recorder = WorkflowRecorder(
                executable=self.executable, archive_directory=archive,
                key_file=output / "site-1" / "synthetic-key.seed",
                policy_path=output / "receiver-policy.json",
                **{field: event[field] for field in ("agent_id", "project_id", "job_id",
                    "configuration_id", "model_id", "framework", "framework_version")})
            self.assertEqual(recorder.latest_round, 3)
            participant = Participant(recorder, event["configuration_id"], allow_in_process_messages=True)
            calls = []
            def train(message, context):
                calls.append(1)
                return response(message)
            callback = participant.wrap(train)
            with self.assertRaises(RoundRejected):
                callback(request(configuration_id=event["configuration_id"]), context())
            self.assertEqual(calls, [])
            self.assertFalse(callback(request(server_round=4,
                             configuration_id=event["configuration_id"]), context()).has_error())
            self.assertEqual(calls, [1])
            self.assertEqual(recorder.latest_round, 4)

    def test_failed_and_cancelled_real_clients_never_export_success_or_error_contents(self):
        marker = "PRIVATE_TRAINING_MARKER_NEVER_EXPORT"
        for exception, kind in ((RuntimeError(marker), "work_failed"),
                                (KeyboardInterrupt(marker), "work_cancelled")):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "demo"
                def failing_client(participant, node_id):
                    app = ClientApp()
                    @app.train()
                    @participant.wrap
                    def train(message, context):
                        raise exception
                    return app, context(node_id)
                with patch("demo.make_client", side_effect=failing_client):
                    with self.assertRaisesRegex(RuntimeError, "training_round_failed"):
                        run_demo(self.executable, output, "manufacturing", True)
                events = []
                for path in output.rglob("*"):
                    if path.is_file():
                        self.assertNotIn(marker.encode(), path.read_bytes())
                    if path.parent.name == "receipts" and path.suffix == ".json":
                        events.append(json.loads(path.read_text())["event"])
                self.assertEqual(sum(e["event_type"] == kind for e in events),
                                 4 if kind == "work_failed" else 3)
                self.assertNotIn("work_completed", {e["event_type"] for e in events})
                self.assertFalse((output / "synthetic-model.npy").exists())

    def test_no_release_without_explicit_approval_and_existing_directory_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "demo"
            summary = run_demo(self.executable, output, "manufacturing")
            self.assertEqual(summary["receipt_count"], 13)
            self.assertFalse((output / "synthetic-model.npy").exists())
            with self.assertRaises(FileExistsError):
                run_demo(self.executable, output, "pharma", True)


if __name__ == "__main__":
    unittest.main()

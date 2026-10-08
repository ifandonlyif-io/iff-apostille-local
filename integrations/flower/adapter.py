"""Optional Flower 1.39 adapter: local metadata receipts, never model updates.

Import this module before importing Flower in the application entry point.
This adapter does not start a Flower server, install dependencies, or authenticate
nodes; deployment transport and node authentication remain Flower responsibilities.
"""
from __future__ import annotations

import asyncio
from dataclasses import dataclass
from importlib.metadata import version
from concurrent.futures import CancelledError
import os
import sys
import threading
from typing import Callable, Iterable

# Refuse a too-late import which would leave cached telemetry settings enabled.
_telemetry = sys.modules.get("flwr.supercore.telemetry")
if _telemetry is not None and getattr(_telemetry, "FLWR_TELEMETRY_ENABLED", "1") != "0":
    raise RuntimeError("import_adapter_before_flower_or_disable_telemetry_at_startup")

# The adapter is opt-in, but telemetry/runtime installation are not. Set before
# importing Flower (whose telemetry configuration is captured at import time).
os.environ["FLWR_TELEMETRY_ENABLED"] = "0"
os.environ["FLWR_DISABLE_RUNTIME_DEPENDENCY_INSTALLATION"] = "1"

if version("flwr") != "1.39.0":
    raise RuntimeError("unsupported_flower_version")

import numpy as np
from flwr.app import ArrayRecord, ConfigRecord, Context, Error, Message, MetricRecord
from flwr.serverapp.strategy import FedAvg
from apostille_local.workflow import ReceiptOutcome, WorkflowRecorder


class RoundRejected(ValueError):
    """Fixed, non-sensitive invalid/replayed/out-of-order request error."""


def _record(recorder: WorkflowRecorder, event_type: str, server_round: int,
            configuration_id: str) -> ReceiptOutcome:
    # An unexpected signing implementation error must not discard completed work.
    # Do not include exception text: a subprocess/runtime may embed private inputs.
    try:
        _configuration_matches(recorder, configuration_id)
        return recorder.record(event_type, round=server_round)
    except Exception:
        return ReceiptOutcome(status="failed", receipt_path=None, event_id="", error="signing_failed")


def _configuration_matches(recorder: WorkflowRecorder, configuration_id: str) -> None:
    if configuration_id != recorder.configuration_id:
        raise ValueError("configuration_mismatch")


@dataclass(frozen=True)
class _ReplyExpectation:
    request: Message
    run_id: int
    node_id: int
    coordinator_id: int
    coordinator_task_id: int | None
    client_task_id: int | None
    request_id: str
    group_id: str
    message_type: str

    @classmethod
    def capture(cls, request: Message):
        meta = request.metadata
        return cls(request, meta.run_id, meta.dst_node_id, meta.src_node_id,
                   meta.src_task_id, meta.dst_task_id, meta.message_id,
                   meta.group_id, meta.message_type)

    def matches(self, reply: Message, allow_empty_request_id: bool = False) -> bool:
        meta = reply.metadata
        # Keep the request object: Flower transport can assign message/task IDs
        # after configure_train returns. All pre-dispatch routing is snapshotted.
        dispatched = self.request.metadata
        request_id = self.request_id or dispatched.message_id
        client_task = self.client_task_id if self.client_task_id is not None else dispatched.dst_task_id
        return ((bool(request_id) or allow_empty_request_id)
                and meta.run_id == self.run_id and meta.src_node_id == self.node_id
                and meta.dst_node_id == self.coordinator_id
                and meta.dst_task_id == self.coordinator_task_id
                and meta.group_id == self.group_id and meta.message_type == self.message_type
                and meta.reply_to_message_id == request_id
                and (client_task is None or meta.src_task_id == client_task))


def _latest_round(recorder: WorkflowRecorder, configuration_id: str) -> int:
    try:
        _configuration_matches(recorder, configuration_id)
        latest = recorder.latest_round
        if type(latest) is not int or latest < 0:
            raise ValueError("invalid_round")
        return latest
    except Exception:
        raise RoundRejected("receipt_archive_invalid") from None


def _valid_arrays(arrays: ArrayRecord) -> bool:
    values = arrays.to_numpy_ndarrays()
    return bool(values) and all(a.size > 0 and np.issubdtype(a.dtype, np.number)
                                and np.isfinite(a).all() for a in values)


class Participant:
    """Wrap a real ClientApp train callback for one job/participant.

    Return values are untouched on success, including when signing fails.
    Failures become Flower Error messages with fixed reasons. Inspect last_receipt
    separately; it does not authorize retrying training. Use one instance per job.
    """

    def __init__(self, recorder: WorkflowRecorder, configuration_id: str, *,
                 allow_in_process_messages: bool = False):
        _configuration_matches(recorder, configuration_id)
        self.recorder = recorder
        self.configuration_id = configuration_id
        self.allow_in_process_messages = allow_in_process_messages
        self.last_receipt: ReceiptOutcome | None = None
        self._last_round = 0
        self._lock = threading.Lock()

    def wrap(self, train: Callable[[Message, Context], Message]) -> Callable[[Message, Context], Message]:
        def wrapped(message: Message, context: Context) -> Message:
            with self._lock:
                self._last_round = max(self._last_round, _latest_round(self.recorder, self.configuration_id))
                try:
                    config = message.content.config_records["config"]
                    server_round = config["server-round"]
                    approved = config["configuration-id"] == self.configuration_id
                except (KeyError, TypeError, ValueError):
                    raise RoundRejected("round_rejected") from None
                if (type(server_round) is not int or server_round != self._last_round + 1
                        or not approved or message.metadata.group_id != str(server_round)
                        or message.metadata.dst_node_id != context.node_id
                        or message.metadata.run_id != context.run_id
                        or (not self.allow_in_process_messages and not message.metadata.message_id)):
                    raise RoundRejected("round_rejected")
                # Consume before executing. A failed/cancelled attempt cannot be
                # replayed with fresh event IDs to produce another success receipt.
                self._last_round = server_round
                expected = _ReplyExpectation.capture(message)
                try:
                    result = train(message, context)
                    if result.has_error():
                        self.last_receipt = _record(self.recorder, "work_failed", server_round, self.configuration_id)
                        return Message(error=Error(1, "training_failed"), reply_to=message)
                    if not expected.matches(result, self.allow_in_process_messages):
                        raise ValueError("invalid_training_result")
                    arrays = result.content.array_records
                    if len(arrays) != 1 or not _valid_arrays(next(iter(arrays.values()))):
                        raise ValueError("invalid_training_result")
                except (KeyboardInterrupt, asyncio.CancelledError, CancelledError):
                    self.last_receipt = _record(self.recorder, "work_cancelled", server_round, self.configuration_id)
                    return Message(error=Error(2, "training_cancelled"), reply_to=message)
                except Exception:
                    self.last_receipt = _record(self.recorder, "work_failed", server_round, self.configuration_id)
                    return Message(error=Error(1, "training_failed"), reply_to=message)
                self.last_receipt = _record(self.recorder, "work_completed", server_round, self.configuration_id)
                return result
        return wrapped


class EvidenceFedAvg(FedAvg):
    """FedAvg with strict all-participant completion and local receipt hooks.

    Standard FedAvg can aggregate partial rounds; this preview deliberately
    requires the configured participant set. The set is a scheduling constraint,
    not proof of the participants' identity or honest execution.
    """

    def __init__(self, recorder: WorkflowRecorder, configuration_id: str,
                 expected_node_ids: Iterable[int], *,
                 allow_in_process_messages: bool = False):
        self.expected_node_ids = frozenset(expected_node_ids)
        if not self.expected_node_ids or any(type(n) is not int or n <= 0 for n in self.expected_node_ids):
            raise ValueError("invalid_participant_set")
        super().__init__(fraction_train=1.0, min_train_nodes=len(self.expected_node_ids),
                         min_available_nodes=len(self.expected_node_ids))
        _configuration_matches(recorder, configuration_id)
        self.recorder = recorder
        self.configuration_id = configuration_id
        self.allow_in_process_messages = allow_in_process_messages
        self.last_receipt: ReceiptOutcome | None = None
        self._last_round = 0
        self._pending_round: int | None = None
        self._expected_replies: dict[int, _ReplyExpectation] = {}
        self._lock = threading.Lock()

    def configure_train(self, server_round: int, arrays: ArrayRecord,
                        config: ConfigRecord, grid) -> Iterable[Message]:
        with self._lock:
            self._last_round = max(self._last_round, _latest_round(self.recorder, self.configuration_id))
            if (type(server_round) is not int or server_round != self._last_round + 1
                    or self._pending_round is not None):
                raise RoundRejected("round_rejected")
            if set(grid.get_node_ids()) != self.expected_node_ids:
                raise RoundRejected("participant_set_rejected")
            # A fresh config prevents callers changing the approval ID after
            # configure_train has handed messages to a transport.
            outgoing = ConfigRecord(dict(config))
            outgoing["configuration-id"] = self.configuration_id
            messages = list(super().configure_train(server_round, arrays, outgoing, grid))
            for message in messages:
                message.metadata.group_id = str(server_round)
            self._expected_replies = {message.metadata.dst_node_id: _ReplyExpectation.capture(message)
                                      for message in messages}
            self._pending_round = server_round
            return messages

    def aggregate_train(self, server_round: int, replies: Iterable[Message]
                        ) -> tuple[ArrayRecord | None, MetricRecord | None]:
        with self._lock:
            if self._pending_round != server_round or type(server_round) is not int:
                raise RoundRejected("round_rejected")
            self._pending_round = None
            self._last_round = server_round
            try:
                replies = list(replies)
                # Reject errors before calling FedAvg: its default error logger
                # prints Error.reason, which could contain private client data.
                if (len(replies) != len(self.expected_node_ids)
                        or {m.metadata.src_node_id for m in replies} != self.expected_node_ids
                        or any(m.has_error() or not self._expected_replies[m.metadata.src_node_id].matches(m, self.allow_in_process_messages)
                               for m in replies)):
                    raise ValueError("incomplete_round")
                arrays, metrics = super().aggregate_train(server_round, replies)
                if arrays is None or not _valid_arrays(arrays):
                    raise ValueError("missing_aggregate")
            except (KeyboardInterrupt, asyncio.CancelledError, CancelledError):
                self.last_receipt = _record(self.recorder, "work_cancelled", server_round, self.configuration_id)
                return None, None
            except Exception:
                self.last_receipt = _record(self.recorder, "work_failed", server_round, self.configuration_id)
                return None, None
            self.last_receipt = _record(self.recorder, "work_completed", server_round, self.configuration_id)
            return arrays, metrics

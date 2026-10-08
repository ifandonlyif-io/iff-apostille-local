"""Local, metadata-only workflow receipts using the Go offline signer.

The recorder never implements cryptography, reads signing key contents, or invokes
training. Keep each agent's archive on a local filesystem with working POSIX
locks. A receipt failure is separate from the result of the training operation;
callers must not repeat training to retry a receipt.
"""

from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
try:
    import fcntl
except ImportError:  # Keep the HTTP SDK importable on non-POSIX clients.
    fcntl = None
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import time
from typing import Iterator
import uuid


_SCHEMA = "urn:apostille:workflow-event:0.1"
_SCOPE = "workflow_metadata_only"
_MAX_INTEGER = 2**53 - 1
_MAX_RECEIPT_BYTES = 64 * 1024
_WORK = frozenset({"work_completed", "work_failed", "work_cancelled"})
_ARTIFACT = frozenset({"model_released", "deployment_accepted"})
_EVENTS = _WORK | _ARTIFACT | {"configuration_approved"}
_FIELDS = frozenset({"schema", "evidence_scope", "project_id", "job_id", "agent_id",
                     "event_id", "configuration_id", "model_id", "sequence", "round",
                     "event_type", "framework", "framework_version", "artifact_sha256",
                     "artifact_size"})


@dataclass(frozen=True)
class ReceiptOutcome:
    """Signing outcome only; a failed receipt never means training was undone."""

    status: str
    receipt_path: Path | None
    event_id: str
    error: str | None = None


class _Failure(Exception):
    def __init__(self, code: str):
        self.code = code


def _uuid(value: object) -> bool:
    if not isinstance(value, str):
        return False
    try:
        parsed = uuid.UUID(value)
        return parsed.version == 4 and str(parsed) == value
    except (ValueError, AttributeError):
        return False


def _absolute(value: os.PathLike[str] | str) -> Path:
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts:
        raise _Failure("invalid_configuration")
    return path


def _private_file(path: Path) -> os.stat_result:
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) & 0o077 or info.st_nlink != 1):
        raise _Failure("unsafe_file")
    return info


def _read_receipt(path: Path) -> dict:
    info = _private_file(path)
    if info.st_size > _MAX_RECEIPT_BYTES:
        raise _Failure("archive_invalid")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        opened = os.fstat(descriptor)
        if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
            raise _Failure("archive_invalid")
        with os.fdopen(descriptor, "rb", closefd=False) as source:
            raw = source.read(_MAX_RECEIPT_BYTES + 1)
        if len(raw) > _MAX_RECEIPT_BYTES:
            raise _Failure("archive_invalid")
        receipt = json.loads(raw)
    finally:
        os.close(descriptor)
    if not isinstance(receipt, dict) or set(receipt) != {"event", "bundle"}:
        raise _Failure("archive_invalid")
    event = receipt["event"]
    if (not isinstance(event, dict) or set(event) != _FIELDS
            or any(not isinstance(value, str) for value in event.values())):
        raise _Failure("archive_invalid")
    return event


class WorkflowRecorder:
    """Sign one agent's workflow events with an independently pinned policy.

    All paths are explicit and absolute. The dedicated archive is created with
    mode 0700 if absent; existing archives must already have that mode. The
    policy may authorize multiple agents, but this archive accepts only this
    recorder's agent, project and job. Resumption verifies existing signatures
    before deriving sequence and terminal-round guards. Deleting an archive's
    suffix is not detectable: these receipts do not prove log completeness.
    """

    def __init__(self, *, executable: os.PathLike[str] | str,
                 archive_directory: os.PathLike[str] | str,
                 key_file: os.PathLike[str] | str,
                 policy_path: os.PathLike[str] | str,
                 agent_id: str, project_id: str, job_id: str,
                 configuration_id: str, model_id: str,
                 framework: str, framework_version: str,
                 timeout: float = 30.0):
        try:
            if fcntl is None:
                raise _Failure("unsupported_platform")
            self.executable = _absolute(executable)
            self.archive_directory = _absolute(archive_directory)
            self.key_file = _absolute(key_file)
            self.policy_path = _absolute(policy_path)
            identifiers = (agent_id, project_id, job_id, configuration_id, model_id)
            if not all(_uuid(value) for value in identifiers):
                raise _Failure("invalid_configuration")
            if (not isinstance(framework, str)
                    or re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", framework) is None
                    or not isinstance(framework_version, str)
                    or re.fullmatch(r"[0-9][A-Za-z0-9._+-]{0,31}", framework_version) is None
                    or isinstance(timeout, bool) or not isinstance(timeout, (int, float))
                    or not 0 < timeout <= 300):
                raise _Failure("invalid_configuration")
            executable_info = self.executable.lstat()
            if (not stat.S_ISREG(executable_info.st_mode)
                    or not os.access(self.executable, os.X_OK)):
                raise _Failure("invalid_configuration")
            self.agent_id, self.project_id, self.job_id = agent_id, project_id, job_id
            self.configuration_id, self.model_id = configuration_id, model_id
            self.framework, self.framework_version = framework, framework_version
            self.timeout = float(timeout)
            _private_file(self.key_file)  # Never read the signing key in Python.
            # The policy contains public pins; unlike a key it may be world-readable.
            if not stat.S_ISREG(self.policy_path.lstat().st_mode):
                raise _Failure("invalid_configuration")
            self.archive_directory.mkdir(mode=0o700, exist_ok=True)
            self._archive_identity = self._check_archive()
            with self._locked():
                self._state()
        except (_Failure, OSError, ValueError, TypeError, OverflowError) as exc:
            code = exc.code if isinstance(exc, _Failure) else "invalid_configuration"
            raise ValueError(code) from None

    def _check_archive(self) -> tuple[int, int]:
        info = self.archive_directory.lstat()
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                or stat.S_IMODE(info.st_mode) != 0o700):
            raise _Failure("unsafe_archive")
        identity = (info.st_dev, info.st_ino)
        if hasattr(self, "_archive_identity") and identity != self._archive_identity:
            raise _Failure("unsafe_archive")
        return identity

    @contextmanager
    def _locked(self) -> Iterator[None]:
        self._check_archive()
        descriptor = os.open(self.archive_directory / ".lock",
                             os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600)
        try:
            info = os.fstat(descriptor)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or stat.S_IMODE(info.st_mode) & 0o077 or info.st_nlink != 1):
                raise _Failure("unsafe_archive")
            deadline = time.monotonic() + self.timeout
            while True:
                try:
                    fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise _Failure("archive_busy")
                    time.sleep(min(0.02, self.timeout))
            self._check_archive()
            yield
        finally:
            os.close(descriptor)

    def _command(self, *arguments: str, failure: str) -> dict:
        try:
            result = subprocess.run([str(self.executable), *arguments],
                                    stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, check=False,
                                    timeout=self.timeout, env={"LANG": "C", "LC_ALL": "C"})
        except subprocess.TimeoutExpired:
            raise _Failure("signer_timeout") from None
        except OSError:
            raise _Failure(failure) from None
        if result.returncode != 0 or len(result.stdout) > 64 * 1024:
            raise _Failure(failure)
        try:
            output = json.loads(result.stdout)
        except (ValueError, UnicodeError):
            raise _Failure(failure) from None
        if not isinstance(output, dict):
            raise _Failure(failure)
        return output

    def _state(self) -> tuple[int, set[str]]:
        # Reject symlinks and unknown files before asking the verifier to open them.
        paths = []
        for path in self.archive_directory.iterdir():
            if path.name == ".lock":
                continue
            if path.suffix != ".json" or not _uuid(path.stem):
                raise _Failure("archive_invalid")
            _private_file(path)
            paths.append(path)
        verified = self._command("verify-set", "--directory", str(self.archive_directory),
                                 "--policy", str(self.policy_path), failure="archive_invalid")
        if (verified.get("valid") is not True or verified.get("record_count") != len(paths)
                or verified.get("project_id") != self.project_id
                or verified.get("job_id") != self.job_id):
            raise _Failure("archive_invalid")
        sequences: set[int] = set()
        rounds: set[str] = set()
        for path in paths:
            event = _read_receipt(path)
            if (event["project_id"] != self.project_id or event["job_id"] != self.job_id
                    or event["agent_id"] != self.agent_id or event["event_id"] != path.stem
                    or event["schema"] != _SCHEMA or event["evidence_scope"] != _SCOPE
                    or re.fullmatch(r"[1-9][0-9]{0,15}", event["sequence"]) is None):
                raise _Failure("archive_invalid")
            sequence = int(event["sequence"])
            if sequence > _MAX_INTEGER or sequence in sequences:
                raise _Failure("archive_invalid")
            sequences.add(sequence)
            if (event["configuration_id"] == self.configuration_id
                    and event["model_id"] == self.model_id
                    and event["framework"] == self.framework
                    and event["event_type"] in _WORK):
                if event["round"] in rounds:
                    raise _Failure("archive_invalid")
                rounds.add(event["round"])
        if sequences and (min(sequences) != 1 or max(sequences) != len(sequences)):
            raise _Failure("archive_invalid")
        self._check_archive()
        return len(sequences), rounds

    @property
    def latest_round(self) -> int:
        """Highest verified terminal round in the current configuration stream.

        Framework adapters may use this to reject old rounds before training.
        The caller must own training exclusively: the archive lock protects
        receipt writing and does not make model execution exactly-once. If
        signing failed, the framework's own checkpoint remains authoritative.
        """
        try:
            with self._locked():
                _, rounds = self._state()
                return max((int(value) for value in rounds), default=0)
        except (_Failure, OSError, ValueError, TypeError, OverflowError) as exc:
            code = exc.code if isinstance(exc, _Failure) else "archive_invalid"
            raise ValueError(code) from None

    def record(self, event_type: str, *, round: int | str | None = None,
               artifact_path: os.PathLike[str] | str | None = None) -> ReceiptOutcome:
        """Record a completed lifecycle action without executing or retrying it."""
        event_id = str(uuid.uuid4())
        try:
            if not isinstance(event_type, str) or event_type not in _EVENTS:
                raise _Failure("invalid_event")
            round_text = ""
            if event_type in _WORK:
                if isinstance(round, bool) or not isinstance(round, (int, str)):
                    raise _Failure("invalid_round")
                round_text = str(round)
                if (re.fullmatch(r"[1-9][0-9]{0,15}", round_text) is None
                        or int(round_text) > _MAX_INTEGER):
                    raise _Failure("invalid_round")
            elif round is not None:
                raise _Failure("invalid_round")
            artifact = None
            if artifact_path is not None:
                if event_type not in _ARTIFACT:
                    raise _Failure("artifact_not_allowed")
                artifact = _absolute(artifact_path)
                if not stat.S_ISREG(artifact.lstat().st_mode):
                    raise _Failure("invalid_artifact")
            _private_file(self.key_file)
            with self._locked():
                sequence, rounds = self._state()
                if round_text and round_text in rounds:
                    raise _Failure("round_already_recorded")
                if round_text and int(round_text) <= max((int(value) for value in rounds), default=0):
                    raise _Failure("round_not_increasing")
                if sequence >= _MAX_INTEGER:
                    raise _Failure("sequence_exhausted")
                event = {"schema": _SCHEMA, "evidence_scope": _SCOPE,
                         "project_id": self.project_id, "job_id": self.job_id,
                         "agent_id": self.agent_id, "event_id": event_id,
                         "configuration_id": self.configuration_id, "model_id": self.model_id,
                         "sequence": str(sequence + 1), "round": round_text,
                         "event_type": event_type, "framework": self.framework,
                         "framework_version": self.framework_version,
                         "artifact_sha256": "", "artifact_size": ""}
                # A private subdirectory stays outside the root receipt scan and
                # on the same filesystem even when the archive is a mount point.
                # An interrupted leftover directory makes the next scan fail closed.
                with tempfile.TemporaryDirectory(prefix=".apostille-workflow-",
                                                 dir=self.archive_directory) as temporary:
                    pending = Path(temporary)
                    event_path, signed_path = pending / "event.json", pending / "receipt.json"
                    descriptor = os.open(event_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                    with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                        json.dump(event, output, separators=(",", ":"), sort_keys=True)
                    extra = ["--artifact", str(artifact)] if artifact is not None else []
                    signed = self._command("sign", "--event", str(event_path), "--key-file",
                                           str(self.key_file), "--agent-id", self.agent_id,
                                           "--out", str(signed_path), *extra, failure="signing_failed")
                    if signed.get("status") != "ready" or signed.get("event_id") != event_id:
                        raise _Failure("signing_failed")
                    verified = self._command("verify", "--receipt", str(signed_path),
                                             "--policy", str(self.policy_path), *extra,
                                             failure="receipt_invalid")
                    if verified.get("valid") is not True:
                        raise _Failure("receipt_invalid")
                    published_event = _read_receipt(signed_path)
                    compare = dict(published_event)
                    compare["artifact_sha256"], compare["artifact_size"] = "", ""
                    if (compare != event or (artifact is None and
                            (published_event["artifact_sha256"] or published_event["artifact_size"]))):
                        raise _Failure("receipt_invalid")
                    self._check_archive()
                    destination = self.archive_directory / f"{event_id}.json"
                    # Link publishes exclusively and atomically on this filesystem.
                    # A signer failure never reserves a sequence or enters the archive.
                    os.link(signed_path, destination, follow_symlinks=False)
                    signed_path.unlink()
                    try:
                        with destination.open("rb") as receipt:
                            os.fsync(receipt.fileno())
                        directory_fd = os.open(self.archive_directory, os.O_RDONLY | os.O_DIRECTORY)
                        try:
                            os.fsync(directory_fd)
                        finally:
                            os.close(directory_fd)
                    except OSError:
                        destination.unlink()
                        raise
                return ReceiptOutcome("ready", destination, event_id)
        except (_Failure, OSError, ValueError, TypeError, OverflowError) as exc:
            code = exc.code if isinstance(exc, _Failure) else "recording_failed"
            return ReceiptOutcome("failed", None, event_id, code)


# A short name is convenient for application code; both names denote the same API.
Recorder = WorkflowRecorder

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
import time
from typing import Iterator
import uuid


_CORE_03 = "https://ifandonlyif.io/apostille/spec/0.3"
_SCHEMA = "urn:apostille:workflow-event:0.1"
_SCOPE = "workflow_metadata_only"
_MAX_INTEGER = 2**53 - 1
_MAX_RECEIPT_BYTES = 64 * 1024
_MAX_ATTEMPT_STREAMS = 128
_MAX_RECOVERY_STAGES = 32
_ATTEMPT_SCHEMA = "urn:apostille:workflow-attempt-state:0.1"
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
    warning: str | None = None


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


def _private_file(path: Path, links: tuple[int, ...] = (1,)) -> os.stat_result:
    info = path.lstat()
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) & 0o077 or info.st_nlink not in links):
        raise _Failure("unsafe_file")
    return info


def _read_private(path: Path, links: tuple[int, ...] = (1,)) -> bytes:
    info = _private_file(path, links)
    if info.st_size > _MAX_RECEIPT_BYTES:
        raise _Failure("archive_invalid")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        opened = os.fstat(descriptor)
        if ((opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino)
                or opened.st_nlink not in links):
            raise _Failure("archive_invalid")
        with os.fdopen(descriptor, "rb", closefd=False) as source:
            raw = source.read(_MAX_RECEIPT_BYTES + 1)
        if len(raw) > _MAX_RECEIPT_BYTES:
            raise _Failure("archive_invalid")
    finally:
        os.close(descriptor)
    return raw


def _read_receipt(path: Path, links: tuple[int, ...] = (1,)) -> dict:
    receipt = json.loads(_read_private(path, links))
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
    mode 0700 if absent; existing archives must already have that mode. The key
    file is read only by the Go CLI: an Apostille JSON key file (ML-DSA-65 signs
    Core 0.3) or a legacy classical raw Ed25519 seed (signs Core 0.1). Verification
    accepts Core 0.1 and Core 0.3 unless ``require_post_quantum=True``, which passes
    ``--require-post-quantum`` and accepts only Core 0.3 (ML-DSA-65). The
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
                 timeout: float = 30.0, require_post_quantum: bool = False):
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
                    or not 0 < timeout <= 300 or not isinstance(require_post_quantum, bool)):
                raise _Failure("invalid_configuration")
            self.require_post_quantum = require_post_quantum
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
                self._attempts()
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

    def _verify_options(self) -> tuple[str, ...]:
        return ("--require-post-quantum",) if self.require_post_quantum else ()

    def _check_post_quantum(self, verified: dict, failure: str) -> None:
        """Defense in depth: do not trust that the Go CLI honoured the flag."""
        if not self.require_post_quantum:
            return
        if "core_protocols" in verified:
            found = verified["core_protocols"]
            ok = isinstance(found, list) and all(item == _CORE_03 for item in found)
        else:
            ok = verified.get("core_protocol") == _CORE_03
        if not ok:
            raise _Failure(failure)

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

    @staticmethod
    def _cleanup_staging(pending: Path) -> bool:
        # Caller either created this directory or validated its committed receipt.
        # Stop at the first failure so recovery retains the remaining correlation.
        try:
            for name in ("receipt.json", "event.json"):
                try:
                    (pending / name).unlink()
                except FileNotFoundError:
                    pass
            pending.rmdir()
            return True
        except OSError:
            return False

    def _recover_publications(self) -> tuple[set[str], set[Path]]:
        stages, linked = set(), set()
        examined = 0
        for pending in self.archive_directory.iterdir():
            if not pending.name.startswith(".apostille-workflow-"):
                continue
            event_id = pending.name.removeprefix(".apostille-workflow-")
            examined += 1
            if not _uuid(event_id) or examined > _MAX_RECOVERY_STAGES:
                raise _Failure("archive_invalid")
            info = pending.lstat()
            if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid()
                    or stat.S_IMODE(info.st_mode) != 0o700):
                raise _Failure("archive_invalid")
            names = {entry.name for entry in pending.iterdir()}
            if not names <= {"event.json", "receipt.json"}:
                raise _Failure("archive_invalid")
            destination = self.archive_directory / f"{event_id}.json"
            dest_info = _private_file(destination, (1, 2))
            source = pending / "receipt.json"
            if "receipt.json" in names:
                source_info = _private_file(source, (2,))
                if (source_info.st_dev, source_info.st_ino) != (dest_info.st_dev, dest_info.st_ino):
                    raise _Failure("archive_invalid")
            elif dest_info.st_nlink != 1:
                raise _Failure("archive_invalid")
            # Only an already committed, independently verified receipt can
            # authorize recovery. An interrupted pre-publication stage is kept.
            verified = self._command("verify", "--receipt", str(destination),
                                     "--policy", str(self.policy_path), *self._verify_options(),
                                     failure="archive_invalid")
            self._check_post_quantum(verified, "archive_invalid")
            event = _read_receipt(destination, (1, 2))
            if (verified.get("valid") is not True or event["event_id"] != event_id
                    or event["agent_id"] != self.agent_id or event["project_id"] != self.project_id
                    or event["job_id"] != self.job_id):
                raise _Failure("archive_invalid")
            if "event.json" in names:
                original = json.loads(_read_private(pending / "event.json"))
                expected = dict(event)
                expected["artifact_sha256"], expected["artifact_size"] = "", ""
                if original != expected:
                    raise _Failure("archive_invalid")
            if not self._cleanup_staging(pending):
                stages.add(pending.name)
                if destination.stat().st_nlink == 2:
                    linked.add(destination)
        return stages, linked

    @staticmethod
    def _sync_directory(directory: Path) -> None:
        descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def _parse_attempts(self, path: Path) -> dict:
        def object_pairs(pairs):
            result = {}
            for key, value in pairs:
                if key in result:
                    raise _Failure("attempt_state_invalid")
                result[key] = value
            return result
        try:
            state = json.loads(_read_private(path), object_pairs_hook=object_pairs)
            if (not isinstance(state, dict)
                    or set(state) != {"schema", "agent_id", "project_id", "job_id", "streams"}
                    or state["schema"] != _ATTEMPT_SCHEMA or state["agent_id"] != self.agent_id
                    or state["project_id"] != self.project_id or state["job_id"] != self.job_id
                    or not isinstance(state["streams"], list)
                    or not 1 <= len(state["streams"]) <= _MAX_ATTEMPT_STREAMS):
                raise _Failure("attempt_state_invalid")
            seen = set()
            for stream in state["streams"]:
                if (not isinstance(stream, dict)
                        or set(stream) != {"configuration_id", "model_id", "framework", "round"}
                        or not _uuid(stream["configuration_id"]) or not _uuid(stream["model_id"])
                        or not isinstance(stream["framework"], str)
                        or re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", stream["framework"]) is None
                        or not isinstance(stream["round"], str)
                        or re.fullmatch(r"[1-9][0-9]{0,15}", stream["round"]) is None
                        or int(stream["round"]) > _MAX_INTEGER):
                    raise _Failure("attempt_state_invalid")
                key = (stream["configuration_id"], stream["model_id"], stream["framework"])
                if key in seen:
                    raise _Failure("attempt_state_invalid")
                seen.add(key)
            return state
        except (_Failure, OSError, ValueError, TypeError, OverflowError):
            raise _Failure("attempt_state_invalid") from None

    def _attempts(self) -> dict:
        current = self.archive_directory / ".attempts"
        pending = self.archive_directory / ".attempts.pending"
        state = (self._parse_attempts(current) if current.exists() or current.is_symlink() else
                 {"schema": _ATTEMPT_SCHEMA, "agent_id": self.agent_id,
                  "project_id": self.project_id, "job_id": self.job_id, "streams": []})
        if pending.exists() or pending.is_symlink():
            proposed = self._parse_attempts(pending)
            previous = {(s["configuration_id"], s["model_id"], s["framework"]): int(s["round"])
                        for s in state["streams"]}
            following = {(s["configuration_id"], s["model_id"], s["framework"]): int(s["round"])
                         for s in proposed["streams"]}
            if any(following.get(key, -1) < value for key, value in previous.items()):
                raise _Failure("attempt_state_invalid")
            # Conservatively consume a fully written pending reservation after a
            # crash. This does not say whether training ever started.
            os.replace(pending, current)
            self._sync_directory(self.archive_directory)
            state = proposed
        return state

    def reserve_round(self, round: int) -> None:
        """Durably consume the next local attempt before dispatch/training.

        This unsigned operational state survives signing failures; it is not
        execution evidence or a model checkpoint. A caller chooses recovery and
        supplies the model state. Never reset it to retry work after a crash.
        """
        try:
            if type(round) is not int or not 0 < round <= _MAX_INTEGER:
                raise _Failure("invalid_round")
            with self._locked():
                _, signed = self._state()
                state = self._attempts()
                stream = next((s for s in state["streams"]
                               if s["configuration_id"] == self.configuration_id
                               and s["model_id"] == self.model_id and s["framework"] == self.framework), None)
                high = max(max((int(n) for n in signed), default=0),
                           int(stream["round"]) if stream else 0)
                if round != high + 1:
                    raise _Failure("round_rejected")
                if stream is None:
                    if len(state["streams"]) >= _MAX_ATTEMPT_STREAMS:
                        raise _Failure("attempt_state_full")
                    stream = {"configuration_id": self.configuration_id, "model_id": self.model_id,
                              "framework": self.framework, "round": str(round)}
                    state["streams"].append(stream)
                else:
                    stream["round"] = str(round)
                raw = json.dumps(state, separators=(",", ":"), sort_keys=True).encode()
                if len(raw) > _MAX_RECEIPT_BYTES:
                    raise _Failure("attempt_state_full")
                pending = self.archive_directory / ".attempts.pending"
                descriptor = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, 0o600)
                with os.fdopen(descriptor, "wb") as output:
                    output.write(raw)
                    output.flush()
                    os.fsync(output.fileno())
                os.replace(pending, self.archive_directory / ".attempts")
                self._sync_directory(self.archive_directory)
        except (_Failure, OSError, ValueError, TypeError, OverflowError) as exc:
            code = exc.code if isinstance(exc, _Failure) else "attempt_state_unavailable"
            raise ValueError(code) from None

    def _state(self) -> tuple[int, set[str]]:
        # Reject symlinks and unknown files before asking the verifier to open them.
        stages, linked = self._recover_publications()
        self._pending_cleanup_count = len(stages)
        paths = []
        for path in self.archive_directory.iterdir():
            if path.name == ".lock" or path.name in stages:
                continue
            if path.name in {".attempts", ".attempts.pending"}:
                self._parse_attempts(path)
                continue
            if path.suffix != ".json" or not _uuid(path.stem):
                raise _Failure("archive_invalid")
            _private_file(path, (1, 2) if path in linked else (1,))
            paths.append(path)
        verified = self._command("verify-set", "--directory", str(self.archive_directory),
                                 "--policy", str(self.policy_path), *self._verify_options(),
                                 failure="archive_invalid")
        self._check_post_quantum(verified, "archive_invalid")
        if (verified.get("valid") is not True or verified.get("record_count") != len(paths)
                or verified.get("project_id") != self.project_id
                or verified.get("job_id") != self.job_id):
            raise _Failure("archive_invalid")
        sequences: set[int] = set()
        rounds: set[str] = set()
        for path in paths:
            event = _read_receipt(path, (1, 2) if path in linked else (1,))
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

        This excludes unsigned attempt reservations. Framework adapters use
        reserve_round before work so missing signatures cannot reopen an attempt.
        Neither method supplies a model checkpoint or proves model execution.
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
                if self._pending_cleanup_count >= _MAX_RECOVERY_STAGES:
                    raise _Failure("cleanup_required")
                event = {"schema": _SCHEMA, "evidence_scope": _SCOPE,
                         "project_id": self.project_id, "job_id": self.job_id,
                         "agent_id": self.agent_id, "event_id": event_id,
                         "configuration_id": self.configuration_id, "model_id": self.model_id,
                         "sequence": str(sequence + 1), "round": round_text,
                         "event_type": event_type, "framework": self.framework,
                         "framework_version": self.framework_version,
                         "artifact_sha256": "", "artifact_size": ""}
                # UUID-named staging is recoverable only when a matching
                # independently verified receipt was already published.
                pending = self.archive_directory / f".apostille-workflow-{event_id}"
                pending.mkdir(mode=0o700)
                published = False
                warning = None
                try:
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
                                             "--policy", str(self.policy_path),
                                             *self._verify_options(), *extra,
                                             failure="receipt_invalid")
                    self._check_post_quantum(verified, "receipt_invalid")
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
                    # This is the exclusive publication/commit point. Never
                    # report an uncommitted failure or remove it after this link.
                    os.link(signed_path, destination, follow_symlinks=False)
                    published = True
                    try:
                        with destination.open("rb") as receipt:
                            os.fsync(receipt.fileno())
                        self._sync_directory(self.archive_directory)
                    except OSError:
                        warning = "durability_uncertain"
                finally:
                    cleaned = self._cleanup_staging(pending)
                    if published and not cleaned:
                        warning = "cleanup_pending" if warning is None else "cleanup_pending_durability_uncertain"
                return ReceiptOutcome("ready", destination, event_id, warning=warning)
        except (_Failure, OSError, ValueError, TypeError, OverflowError) as exc:
            code = exc.code if isinstance(exc, _Failure) else "recording_failed"
            return ReceiptOutcome("failed", None, event_id, code)


# A short name is convenient for application code; both names denote the same API.
Recorder = WorkflowRecorder

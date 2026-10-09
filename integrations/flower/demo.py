"""Three synthetic sites using real Flower ClientApps and FedAvg on one CPU.

No Flower transport, Ray, GPU, model download, or training data file is used.
Only explicitly approved final synthetic model bytes may be saved. The receipt
archive is metadata, not a replacement for privacy-preserving aggregation.
"""
from __future__ import annotations

# adapter sets Flower's offline-related environment BEFORE any Flower import.
from adapter import EvidenceFedAvg, Participant

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import numpy as np
from flwr.app import ArrayRecord, ConfigRecord, Context, Message, MetricRecord, RecordDict
from flwr.clientapp import ClientApp
from apostille_local.workflow import WorkflowRecorder


def initialize_in_process_task() -> None:
    # Flower 1.39 normally initializes this process identity in its runtime. The
    # pinned CPU harness supplies only that runtime context, not train/aggregate
    # implementations. A deployment must let its Flower runtime own this state.
    from flwr.supercore.task_identity import TaskIdentity
    TaskIdentity.run_id = 1
    TaskIdentity.node_id = 100
    TaskIdentity.task_id = 1


class InProcessNodes:
    """Only the get_node_ids surface used by FedAvg.configure_train.

    This is an in-process transport substitute, not a Flower deployment runtime.
    Actual Flower creates the Messages, routes ClientApp.train, and aggregates.
    """
    def get_node_ids(self):
        return (1, 2, 3)


def private_json(path: Path, value) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
        json.dump(value, stream, sort_keys=True)
        stream.write("\n")


def cli_json(executable: Path, *arguments: str) -> dict:
    # Neither inherited stdin nor subprocess diagnostics can contain training
    # data. A caller sees a fixed failure, never CLI paths or environment values.
    result = subprocess.run([str(executable), *arguments], stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=30, check=False, env={"LANG": "C", "LC_ALL": "C"})
    if result.returncode:
        raise RuntimeError("workflow_cli_failed")
    return json.loads(result.stdout)


def make_client(participant: Participant, node_id: int) -> tuple[ClientApp, Context]:
    # Data is generated locally and remains in this callback's closure. The
    # partitions deliberately differ, so FedAvg combines real trained models.
    generator = np.random.default_rng(20261009 + node_id)
    features = generator.normal(size=(12 + node_id * 3, 2))
    design = np.column_stack([features, np.ones(features.shape[0])])
    targets = design @ np.array([1.5, -0.75, 0.2])
    app = ClientApp()

    @app.train()
    @participant.wrap
    def train(message: Message, context: Context) -> Message:
        weights = message.content["arrays"].to_numpy_ndarrays()[0].copy()
        for _ in range(8):
            gradient = design.T @ (design @ weights - targets) / len(targets)
            weights -= 0.1 * gradient
        return Message(content=RecordDict({
            "arrays": ArrayRecord([weights]),
            "metrics": MetricRecord({"num-examples": len(targets)}),
        }), reply_to=message)

    context = Context(run_id=1, node_id=node_id, node_config={},
                      state=RecordDict(), run_config={})
    return app, context


def run_demo(executable: Path, output: Path, scenario: str,
             approve_synthetic_release: bool = False) -> dict:
    if not executable.is_absolute() or not executable.is_file():
        raise ValueError("absolute_cli_required")
    if scenario not in {"manufacturing", "pharma"}:
        raise ValueError("unknown_scenario")
    # Refuse existing outputs, including symlinks: no mixing old evidence or keys.
    output = output.absolute()
    output.mkdir(mode=0o700, parents=False, exist_ok=False)
    initialize_in_process_task()
    project_id, job_id, configuration_id, model_id = (str(uuid.uuid4()) for _ in range(4))
    work_types = ["work_completed", "work_failed", "work_cancelled"]
    roles = {
        "site-1": work_types,
        "site-2": work_types,
        "site-3": work_types,
        "coordinator": work_types + ["model_released"],
        "approver": ["configuration_approved"],
        "deployer": ["deployment_accepted"],
    }
    identities = {}
    for role in roles:
        home = output / role
        home.mkdir(mode=0o700)
        key = home / "synthetic-key.json"
        agent_id = str(uuid.uuid4())
        public = cli_json(executable, "keygen", "--out-key", str(key), "--agent-id", agent_id)
        identities[role] = (agent_id, key, public["producer_pin"])
    # This policy is trusted only within the synthetic demo: pins come from our
    # own key ceremony, never an embedded key or a downloaded receipt.
    policy_path = output / "receiver-policy.json"
    policy = {"schema": "urn:apostille:workflow-policy:0.1", "project_id": project_id,
              "job_id": job_id, "producers": [
                  {"agent_id": identities[role][0], "key_id": identities[role][2],
                   "event_types": permissions} for role, permissions in roles.items()]}
    private_json(policy_path, policy)
    recorders = {}
    for role, (agent_id, key, _) in identities.items():
        recorders[role] = WorkflowRecorder(
            executable=executable, archive_directory=output / role / "receipts",
            key_file=key, policy_path=policy_path, agent_id=agent_id,
            project_id=project_id, job_id=job_id, configuration_id=configuration_id,
            model_id=model_id, framework="flower", framework_version="1.39.0")

    outcomes = [recorders["approver"].record("configuration_approved")]
    if outcomes[-1].status != "ready":
        raise RuntimeError("configuration_receipt_failed")
    participants = {node: Participant(recorders[f"site-{node}"], configuration_id, allow_in_process_messages=True)
                    for node in (1, 2, 3)}
    clients = {node: make_client(participants[node], node) for node in (1, 2, 3)}
    strategy = EvidenceFedAvg(recorders["coordinator"], configuration_id, (1, 2, 3),
                              allow_in_process_messages=True)
    arrays = ArrayRecord([np.zeros(3, dtype=np.float64)])
    for server_round in range(1, 4):
        messages = strategy.configure_train(server_round, arrays, ConfigRecord(), InProcessNodes())
        replies = []
        for message in messages:
            node = message.metadata.dst_node_id
            app, context = clients[node]
            replies.append(app(message, context))
            outcomes.append(participants[node].last_receipt)
        arrays, _ = strategy.aggregate_train(server_round, replies)
        outcomes.append(strategy.last_receipt)
        if arrays is None:
            raise RuntimeError("training_round_failed")

    if approve_synthetic_release:
        # Deliberate human CLI opt-in for this synthetic exercise only; successful
        # aggregation by itself never causes a model release or deployment claim.
        if any(outcome.status != "ready" for outcome in outcomes):
            raise RuntimeError("release_requires_complete_evidence")
        artifact = output / "synthetic-model.npy"
        descriptor = os.open(artifact, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            np.save(stream, arrays.to_numpy_ndarrays()[0], allow_pickle=False)
        released = recorders["coordinator"].record("model_released", artifact_path=artifact)
        outcomes.append(released)
        if released.status != "ready":
            raise RuntimeError("model_release_receipt_failed")
        # Receiver independently verifies both the policy pin and exact artifact.
        cli_json(executable, "verify", "--receipt", str(released.receipt_path),
                 "--policy", str(policy_path), "--artifact", str(artifact))
        accepted = recorders["deployer"].record("deployment_accepted", artifact_path=artifact)
        outcomes.append(accepted)
        if accepted.status != "ready":
            raise RuntimeError("deployment_receipt_failed")
        cli_json(executable, "verify", "--receipt", str(accepted.receipt_path),
                 "--policy", str(policy_path), "--artifact", str(artifact))

    # This explicit demo export collects only receipts, never keys or client
    # state. In a deployment each site chooses what to export to its receiver.
    exported = output / "export"
    exported.mkdir(mode=0o700)
    for role in roles:
        archive = output / role / "receipts"
        receipts = list(archive.glob("*.json"))
        if not receipts:
            continue
        cli_json(executable, "verify-set", "--directory", str(archive), "--policy", str(policy_path))
        for receipt in receipts:
            target = exported / receipt.name
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "wb") as stream, receipt.open("rb") as source:
                shutil.copyfileobj(source, stream)
    verification = cli_json(executable, "verify-set", "--directory", str(exported),
                            "--policy", str(policy_path))
    summary = {
        "scenario": scenario, "framework": "flower", "framework_version": "1.39.0",
        "execution": "synthetic_in_process_cpu", "sites": 3, "completed_rounds": 3,
        "receipt_count": verification["record_count"],
        "receipt_status": "ready" if all(item.status == "ready" for item in outcomes) else "failed",
        "synthetic_release_approved": approve_synthetic_release,
        "evidence_scope": "workflow_metadata_only",
    }
    private_json(output / "summary.json", summary)
    return summary


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", required=True, type=Path, help="absolute apostille-workflow binary")
    parser.add_argument("--output", required=True, type=Path, help="new private demo directory")
    parser.add_argument("--scenario", choices=("manufacturing", "pharma"), default="manufacturing")
    parser.add_argument("--approve-synthetic-release", action="store_true",
                        help="explicitly authorize synthetic model export and simulated deployment acceptance")
    args = parser.parse_args()
    try:
        result = run_demo(args.cli, args.output, args.scenario, args.approve_synthetic_release)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError):
        print("synthetic_demo_failed", file=sys.stderr)
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0 if result["receipt_status"] == "ready" else 1


if __name__ == "__main__":
    raise SystemExit(main())

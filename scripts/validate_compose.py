#!/usr/bin/env python3
"""Render both vendor templates using Compose, without a daemon or GPU.

Only synthetic paths/image IDs are used. This checks merged configuration;
it never creates containers, loads images, contacts a runtime, or certifies GPUs.
"""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parent.parent
OFFLINE = ("HF_HUB_OFFLINE", "TRANSFORMERS_OFFLINE", "HF_DATASETS_OFFLINE",
           "HF_HUB_DISABLE_TELEMETRY", "VLLM_NO_USAGE_STATS", "DO_NOT_TRACK")


def require(condition, code):
    if not condition:
        raise ValueError(code)


def render(vendor, directory):
    require(vendor in ("nvidia", "amd"), "unsupported_vendor")
    root = Path(directory)
    for name in ("config", "tls", "evidence", "signing", "model"):
        (root / name).mkdir(exist_ok=True)
    values = {
        "APOSTILLE_GATEWAY_IMAGE": "sha256:" + "a" * 64,
        "APOSTILLE_RUNTIME_IMAGE": "sha256:" + "b" * 64,
        "APOSTILLE_MODEL_BUNDLE": str(root / "model"),
        "APOSTILLE_CONFIG_DIR": str(root / "config"),
        "APOSTILLE_TLS_DIR": str(root / "tls"),
        "APOSTILLE_EVIDENCE_DIR": str(root / "evidence"),
        "APOSTILLE_SIGNING_DIR": str(root / "signing"),
        "APOSTILLE_INGRESS_SUBNET": "172.29.80.0/24",
        "APOSTILLE_INFERENCE_SUBNET": "172.29.81.0/24",
        "APOSTILLE_GPU_DEVICE": "0",
        "APOSTILLE_AMD_RENDER_DEVICE": "/dev/dri/renderD128",
        "APOSTILLE_RENDER_GID": "109", "APOSTILLE_VIDEO_GID": "44",
    }
    # No inherited deployment value may override the synthetic test fixture.
    env = {k: v for k, v in os.environ.items() if not k.startswith(("APOSTILLE_", "COMPOSE_"))}
    env.update(values)
    try:
        result = subprocess.run(
            ["docker", "compose", "--project-name", "apostille-software-check", "--env-file", os.devnull,
             "-f", str(ROOT / "deploy" / ("compose." + vendor + ".yml")), "config", "--format", "json"],
            env=env, capture_output=True, timeout=30, check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError("docker_compose_cli_required") from None
    require(result.returncode == 0, "compose_render_failed")
    require(len(result.stdout) <= 1 << 20, "compose_output_too_large")
    try:
        return json.loads(result.stdout)
    except (ValueError, UnicodeError):
        raise ValueError("invalid_compose_output") from None


def validate(document, vendor):
    services = document["services"]
    require(set(services) == {"gateway", "runtime"}, "unexpected_service")
    gateway, runtime = services["gateway"], services["runtime"]
    require(gateway.get("image") == "sha256:" + "a" * 64 and runtime.get("image") == "sha256:" + "b" * 64, "immutable_image_selection_changed")
    for service in services.values():
        require(service.get("platform") == "linux/amd64", "unexpected_platform")
        require(service.get("read_only") is True and service.get("user") == "10001:10001", "unsafe_process_identity")
        require(not service.get("privileged") and not service.get("cap_add"), "privilege_escalation")
        require(service.get("cap_drop") == ["ALL"], "capabilities_not_dropped")
        require(service.get("security_opt") == ["no-new-privileges:true"], "unsafe_security_options")
        require(not service.get("network_mode") and not service.get("ipc") and not service.get("pid"), "shared_host_namespace")
        require(service.get("pull_policy") == "never", "automatic_image_pull")
        require(service.get("logging", {}).get("driver") == "none", "payload_logging_enabled")
        require(service.get("restart") == "no", "startup_before_firewall")
        require(service.get("dns") == ["127.0.0.1"], "external_dns_configured")
        require(service.get("sysctls", {}).get("net.ipv6.conf.all.disable_ipv6") == "1", "ipv6_not_disabled")
        for mount in service.get("volumes", []):
            require(mount.get("type") == "bind" and not mount.get("bind", {}).get("create_host_path", False), "unsafe_bind_mount")
            require(not mount.get("source", "").endswith("docker.sock"), "docker_socket_mount")
        require(all(not service.get("environment", {}).get(k) for k in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY")), "proxy_configured")
    require(not gateway.get("devices") and not gateway.get("gpus") and not gateway.get("deploy", {}).get("resources", {}).get("reservations", {}).get("devices"), "gateway_gpu_access")
    require(not gateway.get("runtime") and not gateway.get("group_add"), "gateway_gpu_access")
    require(set(gateway["networks"]) == {"ingress", "inference"}, "gateway_networks_changed")
    require(set(runtime["networks"]) == {"inference"} and not runtime.get("ports"), "runtime_public_access")
    ports = gateway.get("ports", [])
    require(len(ports) == 1 and ports[0].get("host_ip") == "127.0.0.1" and ports[0].get("target") == 8443, "unexpected_gateway_listener")
    expected_mounts = ({"/config", "/tls", "/evidence", "/signing"}, {"/config", "/opt/apostille/model", "/deployment/runtime_launcher.py"})
    for service, expected in zip((gateway, runtime), expected_mounts):
        mounts = service.get("volumes", [])
        require(len(mounts) == len(expected) and {m["target"] for m in mounts} == expected, "unexpected_host_mount")
        require(all(m.get("read_only", False) for m in mounts if m["target"] != "/evidence"), "writable_asset_or_secret")
    require(runtime.get("entrypoint") == ["python3", "/deployment/runtime_launcher.py"], "asset_validation_bypassed")
    require(runtime.get("command") == ["/config/config.json", "/opt/apostille/model"], "runtime_arguments_changed")
    require(int(runtime.get("shm_size", 0)) == 8 * 1024 ** 3, "shared_memory_size_changed")
    require(all(runtime["environment"].get(k) == "1" for k in OFFLINE), "offline_settings_lost_in_merge")
    require(runtime["environment"].get("VLLM_LOGGING_LEVEL") == "CRITICAL", "runtime_logging_enabled")
    networks = document["networks"]
    require(set(networks) == {"ingress", "inference"} and networks["inference"].get("internal") is True, "inference_network_public")
    for name in networks:
        require(not networks[name].get("enable_ipv6"), "network_ipv6_enabled")
        require(networks[name]["driver_opts"]["com.docker.network.bridge.name"] == "apl-" + name, "firewall_bridge_mismatch")
    reservations = runtime.get("deploy", {}).get("resources", {}).get("reservations", {}).get("devices", [])
    if vendor == "nvidia":
        require(not runtime.get("devices") and len(reservations) == 1, "nvidia_device_selection")
        selected = reservations[0]
        require(selected.get("driver") == "nvidia" and selected.get("device_ids") == ["0"] and selected.get("capabilities") == ["gpu"] and not selected.get("count"), "nvidia_device_selection")
        require("ROCR_VISIBLE_DEVICES" not in runtime["environment"], "mixed_vendor_settings")
    elif vendor == "amd":
        require(not reservations and not runtime.get("gpus"), "mixed_vendor_settings")
        devices = runtime.get("devices", [])
        expected = {"/dev/kfd", "/dev/dri/renderD128"}
        require(len(devices) == 2 and {d["source"] for d in devices} == expected and all(d["source"] == d["target"] for d in devices), "amd_device_selection")
        require(set(runtime.get("group_add", [])) == {"109", "44"}, "amd_device_groups_missing")
        require(runtime["environment"].get("ROCR_VISIBLE_DEVICES") == "0", "amd_device_selection")
    else:
        raise ValueError("unsupported_vendor")


def mutation_checks(document, vendor):
    """Ensure the checks catch drift in the actual Compose-rendered object."""
    mutations = [
        lambda d: d["services"]["gateway"].update(devices=[{"source": "/dev/kfd"}]),
        lambda d: d["services"]["gateway"]["volumes"].append({"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}),
        lambda d: d["services"]["runtime"].update(ports=[{"target": 8000}]),
        lambda d: d["services"]["runtime"].update(privileged=True),
        lambda d: d["services"]["runtime"].update(pull_policy="always"),
        lambda d: d["services"]["runtime"]["environment"].pop("HF_HUB_OFFLINE"),
        lambda d: d["networks"]["inference"].update(internal=False),
    ]
    if vendor == "amd":
        mutations.append(lambda d: d["services"]["runtime"].update(group_add=[]))
    else:
        mutations.append(lambda d: d["services"]["runtime"]["deploy"]["resources"]["reservations"]["devices"][0].update(device_ids=["0", "1"]))
    for mutate in mutations:
        broken = copy.deepcopy(document)
        mutate(broken)
        try:
            validate(broken, vendor)
        except ValueError:
            continue
        raise ValueError("compose_mutation_not_detected")
    return len(mutations)


def main():
    results = []
    try:
        with tempfile.TemporaryDirectory(prefix="apostille-compose-") as tmp:
            for vendor in ("nvidia", "amd"):
                document = render(vendor, tmp)
                validate(document, vendor)
                count = mutation_checks(document, vendor)
                results.append({"vendor": vendor, "compose_configuration": "passed", "detected_mutations": count,
                                "container_execution": "not_tested", "hardware_acceptance": "unverified"})
    except (ValueError, KeyError, TypeError, OverflowError) as exc:
        # All explicit errors are fixed codes; do not print Compose output/env.
        code = str(exc) if isinstance(exc, ValueError) and str(exc).replace("_", "").isalnum() else "compose_validation_failed"
        print(json.dumps({"result": "failed", "code": code}))
        return 1
    print(json.dumps({"result": "software_configuration_passed", "vendors": results}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

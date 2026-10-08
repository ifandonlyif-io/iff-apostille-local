#!/usr/bin/env python3
"""Print/apply scoped Linux Docker iptables egress rules; never flush global rules.

Run while both services are stopped. Supports only Docker's iptables backend.
The fixed bridge names deliberately restrict this template to one deployment.
"""
import argparse
import ipaddress
import json
import os
import shlex
import subprocess
import sys

V4_FORWARD = "IFF_APL_FORWARD"
V4_INPUT = "IFF_APL_INPUT"
V6 = "IFF_APL_V6"
BRIDGES = ("apl-ingress", "apl-inference")


def networks(ingress, inference):
    nets = [ipaddress.ip_network(x, strict=True) for x in (ingress, inference)]
    if any(n.version != 4 or n.prefixlen != 24 or not n.is_private or n.is_loopback or n.is_link_local for n in nets):
        raise ValueError("use two dedicated private IPv4 /24 networks")
    if nets[0].overlaps(nets[1]):
        raise ValueError("networks must not overlap")
    return nets


def commands(ingress, inference):
    networks(ingress, inference)
    rules = []
    def add(binary, *args):
        rules.append([binary, "--wait", "5", *args])
    for chain in (V4_FORWARD, V4_INPUT):
        add("iptables", "-N", chain)
        add("iptables", "-A", chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN")
    add("iptables", "-A", V4_FORWARD, "-d", inference, "-p", "tcp", "--dport", "8000", "-j", "RETURN")
    add("iptables", "-A", V4_FORWARD, "-j", "REJECT")
    add("iptables", "-A", V4_INPUT, "-j", "REJECT")
    add("ip6tables", "-N", V6)
    add("ip6tables", "-A", V6, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN")
    add("ip6tables", "-A", V6, "-j", "REJECT")
    for bridge in BRIDGES:
        add("iptables", "-I", "DOCKER-USER", "1", "-i", bridge, "-j", V4_FORWARD)
        add("iptables", "-I", "INPUT", "1", "-i", bridge, "-j", V4_INPUT)
        add("ip6tables", "-I", "FORWARD", "1", "-i", bridge, "-j", V6)
        add("ip6tables", "-I", "INPUT", "1", "-i", bridge, "-j", V6)
    return rules


def run(args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout


def preflight(ingress, inference):
    if sys.platform != "linux" or os.geteuid() != 0:
        raise ValueError("apply requires a Linux administrator")
    run(["iptables", "--wait", "5", "-S", "DOCKER-USER"])
    run(["ip6tables", "--wait", "5", "-S", "FORWARD"])
    for binary, chain in (("iptables", V4_FORWARD), ("iptables", V4_INPUT), ("ip6tables", V6)):
        result = subprocess.run([binary, "--wait", "5", "-S", chain], capture_output=True)
        if result.returncode == 0:
            raise ValueError("managed chain already exists; inspect existing rules instead of overwriting")
    for bridge, expected in zip(BRIDGES, (ingress, inference)):
        addresses = json.loads(run(["ip", "-j", "address", "show", "dev", bridge]))
        actual = [str(ipaddress.ip_network(f"{a['local']}/{a['prefixlen']}", strict=False))
                  for device in addresses for a in device["addr_info"] if a["family"] == "inet"]
        if actual != [expected]:
            raise ValueError("bridge subnet does not match deployment")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ingress-subnet", required=True)
    parser.add_argument("--inference-subnet", required=True)
    parser.add_argument("--apply", action="store_true", help="otherwise print only; services must be stopped")
    args = parser.parse_args()
    rules = commands(args.ingress_subnet, args.inference_subnet)
    if not args.apply:
        print("# Review first. Stop services, create Compose networks, then explicitly --apply.")
        for rule in rules:
            print(shlex.join(rule))
        return
    preflight(args.ingress_subnet, args.inference_subnet)
    for rule in rules:
        run(rule)
    print("Scoped rules installed. Run real deny-egress acceptance before serving customer traffic.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError):
        print("egress_policy_failed: keep services stopped; inspect scoped IFF_APL_* rules", file=sys.stderr)
        raise SystemExit(1)

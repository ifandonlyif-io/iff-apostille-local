#!/usr/bin/env python3
"""Synthetic connection-only egress check. Sends no customer data.

Run --expect allow on the host first to establish a positive control, then
--expect deny inside each service's network namespace using a local probe image.
Host/network failures are not evidence of successful firewall isolation.
"""
import argparse
import json
import socket
import sys
from pathlib import Path


def connected(family, target):
    try:
        with socket.socket(family, socket.SOCK_STREAM) as sock:
            sock.settimeout(3)
            sock.connect(target)
        return True
    except OSError:
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--expect", choices=("allow", "deny"), required=True)
    parser.add_argument("--control-report", help="host --expect allow JSON report, required for a conclusive deny result")
    parser.add_argument("--proxy-host", help="customer-designated proxy hostname/IP, never a URL or credentials")
    parser.add_argument("--proxy-port", type=int, help="customer-designated proxy TCP port")
    args = parser.parse_args()
    if bool(args.proxy_host) != bool(args.proxy_port) or (args.proxy_port and not 1 <= args.proxy_port <= 65535):
        parser.error("proxy host and valid port are required together")
    if args.proxy_host and any(c in args.proxy_host for c in "/@?#\\\r\n"):
        parser.error("proxy host must not contain URL syntax or credentials")
    ipv4 = connected(socket.AF_INET, ("1.1.1.1", 443))
    ipv6 = connected(socket.AF_INET6, ("2606:4700:4700::1111", 443, 0, 0))
    # Hard bound DNS as well; Docker's embedded resolver must not forward queries.
    if hasattr(__import__("signal"), "SIGALRM"):
        import signal
        signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError()))
        signal.alarm(5)
    try:
        dns = bool(socket.getaddrinfo("example.com", 443))
    except (OSError, TimeoutError):
        dns = False
    finally:
        if "signal" in locals():
            signal.alarm(0)
    proxy = None
    if args.proxy_host:
        try:
            with socket.create_connection((args.proxy_host, args.proxy_port), timeout=3):
                proxy = True
        except OSError:
            proxy = False
    observed = {"ipv4_outbound": ipv4, "ipv6_outbound": ipv6, "external_dns": dns,
                "proxy_reachable": proxy, "proxy_endpoint": [args.proxy_host, args.proxy_port] if args.proxy_host else None}
    if args.expect == "allow":
        passed = ipv4 and dns and (proxy is not False)
        observed["result"] = "positive_control_available" if passed else "inconclusive_positive_control_failed"
    else:
        control = {}
        if args.control_report:
            try:
                raw = Path(args.control_report).read_bytes()
                if len(raw) <= 8192:
                    control = json.loads(raw)
            except (OSError, ValueError):
                pass
        passed = not any((ipv4, ipv6, dns, proxy))
        qualified = (control.get("result") == "positive_control_available" and
                     control.get("ipv4_outbound") is True and control.get("external_dns") is True)
        if args.proxy_host:
            qualified = qualified and control.get("proxy_reachable") is True and control.get("proxy_endpoint") == observed["proxy_endpoint"]
        observed["ipv6_coverage"] = "tested_with_positive_control" if control.get("ipv6_outbound") is True else "unverified_no_positive_control"
        observed["proxy_coverage"] = "tested_with_positive_control" if args.proxy_host and qualified else "unverified"
        if not passed:
            observed["result"] = "outbound_path_detected"
        elif not qualified:
            observed["result"] = "inconclusive_positive_control_required"
            passed = False
        else:
            observed["result"] = "tested_ipv4_dns_paths_denied"
    print(json.dumps(observed, sort_keys=True))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())

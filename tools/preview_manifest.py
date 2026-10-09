"""Package local software-preview metadata; never claim a runtime/model SBOM."""
import argparse
import hashlib
from email.parser import BytesParser
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import sys
import tomllib
import zipfile
from datetime import datetime, timezone
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]


def versions(root=ROOT):
    """Require one Local release identity; the separately pinned Core is unchanged."""
    sdk = tomllib.loads((root / "sdk/python/pyproject.toml").read_text())["project"]["version"]
    match = re.fullmatch(r"([0-9]+\.[0-9]+\.[0-9]+)a([0-9]+)", sdk)
    if match is None:
        raise SystemExit("invalid_sdk_preview_version")
    release = match[1] + "-alpha." + match[2]
    sources = (
        (root / "internal/gateway/gateway.go", r'^const Version = "([^"\n]+)"$'),
        (root / "Makefile", r'^VERSION \?= ([^\s]+)$'),
        (root / "api/openapi.yaml", r'^  version: ([^\s]+)$'),
    )
    for source, pattern in sources:
        found = re.search(pattern, source.read_text(), re.MULTILINE)
        if found is None or found[1] != release:
            raise SystemExit("local_release_version_mismatch")
    return release, sdk


def output_directory(path, version, root=ROOT):
    out = Path(path).resolve()
    if out.parent != root / "dist" or out.name != version:
        raise SystemExit("output_must_be_dist_release_version")
    return out


def prepare(path, version, root=ROOT):
    release, sdk = versions(root)
    if version != release:
        raise SystemExit("requested_release_version_mismatch")
    out = output_directory(path, version, root)
    try:
        # Exclusive reservation precedes every compiler, package builder and download.
        out.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        raise SystemExit("preview_output_already_exists") from None
    with (out / ".preview-building.json").open("x") as marker:
        json.dump({"software_version": release, "sdk_version": sdk}, marker)
    return out


def finalize(path, version, root=ROOT):
    release_version, sdk_version = versions(root)
    if version != release_version:
        raise SystemExit("requested_release_version_mismatch")
    out = output_directory(path, version, root)
    marker = out / ".preview-building.json"
    if (not marker.is_file() or marker.is_symlink()
            or any((out / name).exists() for name in
                   ("PREVIEW.json", "software.spdx.json", "SHA256SUMS", ".preview-finalizing"))):
        raise SystemExit("preview_output_not_fresh_reservation")
    try:
        reservation = json.loads(marker.read_text())
    except (ValueError, OSError):
        raise SystemExit("invalid_preview_reservation") from None
    if reservation != {"software_version": release_version, "sdk_version": sdk_version}:
        raise SystemExit("invalid_preview_reservation")
    # One finalization attempt only; interrupted builds need a new reviewed output.
    try:
        with (out / ".preview-finalizing").open("x") as claim:
            claim.write(release_version + "\n")
    except FileExistsError:
        raise SystemExit("preview_output_not_fresh_reservation") from None
    for name in ("LICENSE", "README.md", "go.mod", "go.sum"):
        shutil.copyfile(root / name, out / name)
    (out / "sdk/python").mkdir(parents=True, exist_ok=True)
    shutil.copyfile(root / "sdk/python/README.md", out / "sdk/python/README.md")
    shutil.copyfile(root / "sdk/python/requirements-linux-py311.lock", out / "sdk/python/requirements-linux-py311.lock")
    for directory in ("deploy", "docs", "api"):
        for source in sorted((root / directory).rglob("*")):
            if not source.is_file() or source.is_symlink():
                continue
            if source.suffix not in (".md", ".yml", ".yaml", ".py", ".example") and source.name != "Dockerfile" and not source.name.endswith(".schema.json"):
                continue
            target = out / source.relative_to(root)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)

    packages = []
    relationships = []
    licenses = out / "third-party-licenses"
    licenses.mkdir(exist_ok=True)


    def add(name, version, purl, kind):
        identifier = "SPDXRef-" + hashlib.sha256((kind + name).encode()).hexdigest()[:20]
        packages.append({"name": name, "SPDXID": identifier, "versionInfo": version,
                         "downloadLocation": "NOASSERTION", "filesAnalyzed": False,
                         "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
                         "copyrightText": "NOASSERTION", "externalRefs": [{
                             "referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
                             "referenceLocator": purl}]})
        relationships.append({"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES",
                              "relatedSpdxElement": identifier})
        return identifier


    result = subprocess.run(["go", "list", "-deps", "-json", "./cmd/..."], cwd=root,
                            env=dict(os.environ, GOWORK="off", GOOS="linux", GOARCH="amd64", CGO_ENABLED="0"),
                            check=True, capture_output=True, text=True)
    decoder = json.JSONDecoder()
    data = result.stdout.strip()
    seen_modules = set()
    while data:
        package, count = decoder.raw_decode(data)
        data = data[count:].lstrip()
        module = package.get("Module")
        if module is None or module["Path"] in seen_modules:
            continue
        seen_modules.add(module["Path"])
        version = release_version if module.get("Main") else module["Version"]
        name = module["Path"]
        identifier = add(name, version, "pkg:golang/" + quote(name, safe="/") + "@" + version, "go")
        if module.get("Dir") and not module.get("Main"):
            for license_file in Path(module["Dir"]).iterdir():
                if license_file.is_file() and license_file.name.upper().startswith(("LICENSE", "LICENCE", "NOTICE", "COPYING")):
                    shutil.copyfile(license_file, licenses / (identifier + "-" + license_file.name))

    # Inventory the actual shipped wheels, including conditional Python 3.11
    # dependencies which may not be imported on the build host's newer interpreter.
    resolved = []
    for wheel in sorted(out.glob("*/*.whl")):
        with zipfile.ZipFile(wheel) as archive:
            metadata = [name for name in archive.namelist() if name.endswith(".dist-info/METADATA")]
            if len(metadata) != 1:
                raise SystemExit("invalid_wheel_metadata")
            dist = BytesParser().parsebytes(archive.read(metadata[0]))
            normalized = dist["Name"].lower().replace("_", "-")
            version = dist["Version"]
            if normalized == "apostille-local" and version != sdk_version:
                raise SystemExit("sdk_wheel_version_mismatch")
            identifier = add(normalized, version, "pkg:pypi/" + normalized + "@" + version, "python")
            packages[-1]["checksums"] = [{"algorithm": "SHA256", "checksumValue": hashlib.sha256(wheel.read_bytes()).hexdigest()}]
            resolved.append(normalized + "==" + version)
            for item in archive.namelist():
                path = Path(item)
                if not item.endswith("/") and ".dist-info/" in item and any(part.lower().startswith(("license", "licence", "notice", "copying")) for part in path.parts):
                    (licenses / (identifier + "-" + path.name)).write_bytes(archive.read(item))

    go_root = Path(subprocess.check_output(["go", "env", "GOROOT"], text=True).strip())
    shutil.copyfile(go_root / "LICENSE", licenses / "go-standard-library-LICENSE")
    go_version = subprocess.check_output(["go", "env", "GOVERSION"], text=True).strip()
    add("Go standard library", go_version, "pkg:generic/golang@" + go_version, "go-toolchain")
    (out / "python-runtime-versions.txt").write_text("\n".join(sorted(resolved)) + "\n")
    manifest = {"preview": True, "hardware_validated": False,
                "software_version": release_version, "sdk_version": sdk_version,
                "scope": "Modules imported by the Linux amd64 Go commands, Go standard library, and shipped Python wheels; inference image, driver, model and Python interpreter excluded",
                "go_toolchain": subprocess.check_output(["go", "version"], text=True).strip(),
                "python_interpreter": sys.version.split()[0],
                "release_gate": "Qualify actual GPU/image/model and attach complete deployment SBOM and hashed offline wheel set before customer release"}
    (out / "PREVIEW.json").write_text(json.dumps(manifest, indent=2) + "\n")
    sbom = {"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
            "name": "apostille-local-software-preview", "documentNamespace": "https://ifandonlyif.io/spdx/" + hashlib.sha256(json.dumps(packages, sort_keys=True).encode()).hexdigest(),
            "creationInfo": {"created": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                             "creators": ["Tool: apostille-local-preview-manifest-0.1"]},
            "documentComment": manifest["scope"], "packages": packages, "relationships": relationships}
    (out / "software.spdx.json").write_text(json.dumps(sbom, indent=2) + "\n")
    (out / ".preview-finalizing").unlink()
    (out / ".preview-building.json").unlink()
    lines = []
    for file in sorted(out.rglob("*")):
        if file.is_file() and file.name != "SHA256SUMS":
            lines.append(hashlib.sha256(file.read_bytes()).hexdigest() + "  " + file.relative_to(out).as_posix())
    with (out / "SHA256SUMS").open("x") as checksums:
        checksums.write("\n".join(lines) + "\n")
    print("Local software preview prepared; hardware/runtime release gates remain open.")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output")
    parser.add_argument("--version", required=True)
    parser.add_argument("--prepare", action="store_true")
    args = parser.parse_args(argv)
    if args.prepare:
        prepare(args.output, args.version)
    else:
        finalize(args.output, args.version)


if __name__ == "__main__":
    main()

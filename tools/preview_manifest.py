"""Package local software-preview metadata; never claim a runtime/model SBOM."""
import hashlib
from email.parser import BytesParser
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import zipfile
from datetime import datetime, timezone
from urllib.parse import quote

root = Path(__file__).resolve().parents[1]
out = Path(sys.argv[1]).resolve()
if root / "dist" not in out.parents:
    raise SystemExit("output_must_be_inside_dist")
out.mkdir(parents=True, exist_ok=True)
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
    version = module.get("Version", "0.1.0-alpha.1-local")
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
lines = []
for file in sorted(out.rglob("*")):
    if file.is_file() and file.name != "SHA256SUMS":
        lines.append(hashlib.sha256(file.read_bytes()).hexdigest() + "  " + file.relative_to(out).as_posix())
(out / "SHA256SUMS").write_text("\n".join(lines) + "\n")
print("Local software preview prepared; hardware/runtime release gates remain open.")

"""Build a Linux runner bundle from a committed source tree."""

import argparse
import hashlib
import json
import os
import subprocess
import tarfile
from contextlib import ExitStack
from pathlib import Path
from tempfile import NamedTemporaryFile, TemporaryDirectory


def build(revision: str, output: Path, arch: str, runner: str) -> None:
    repo = Path(__file__).resolve().parents[2]
    commit = subprocess.check_output(
        ["git", "rev-parse", "--verify", f"{revision}^{{commit}}"], cwd=repo, text=True
    ).strip()
    if output.exists():
        raise FileExistsError(f"Refusing to replace runner bundle: {output}")
    with ExitStack() as stack:
        source = Path(stack.enter_context(TemporaryDirectory(prefix="harbor-build-")))
        archive = stack.enter_context(NamedTemporaryFile(suffix=".tar"))
        subprocess.run(["git", "archive", commit], cwd=repo, stdout=archive, check=True)
        archive.flush()
        with tarfile.open(archive.name) as tree:
            tree.extractall(source, filter="data")
        binary = source / "metacog-agent"
        subprocess.run(
            [
                "go",
                "build",
                "-trimpath",
                "-buildvcs=false",
                "-o",
                str(binary),
                f"./cmd/{runner}",
            ],
            cwd=source,
            env={**os.environ, "GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"},
            check=True,
        )
        data = binary.read_bytes()
        manifest = {
            "revision": commit,
            "runner": runner,
            "sha256": hashlib.sha256(data).hexdigest(),
            "goos": "linux",
            "goarch": arch,
            "go_version": subprocess.check_output(["go", "version"], text=True).strip(),
        }
        output.mkdir(parents=True)
        (output / "metacog-agent").write_bytes(data)
        (output / "metacog-agent").chmod(0o755)
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Built {commit} ({manifest['sha256']}) in {output}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument(
        "--runner",
        default="metacog-agent",
        help="runner command (default: metacog-agent)",
    )
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), default="amd64")
    args = parser.parse_args()
    build(args.revision, args.output, args.arch, args.runner)

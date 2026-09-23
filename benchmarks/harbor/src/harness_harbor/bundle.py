import hashlib
import json
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class Bundle:
    directory: Path
    revision: str
    sha256: str
    arch: str

    @classmethod
    def load(cls, directory: str) -> "Bundle":
        path = Path(directory).expanduser().resolve()
        manifest = json.loads((path / "manifest.json").read_text())
        for name, length in (("revision", 40), ("sha256", 64)):
            value = manifest[name]
            if len(value) != length or any(c not in "0123456789abcdef" for c in value):
                raise ValueError(f"Invalid bundle {name}")
        if manifest["goos"] != "linux" or manifest["goarch"] not in ("amd64", "arm64"):
            raise ValueError("Runner bundle must target Linux amd64 or arm64")
        bundle = cls(path, manifest["revision"], manifest["sha256"], manifest["goarch"])
        bundle.read_binary()
        return bundle

    def read_binary(self) -> bytes:
        data = (self.directory / "heuristic").read_bytes()
        if hashlib.sha256(data).hexdigest() != self.sha256:
            raise ValueError("Runner binary checksum does not match its manifest")
        return data

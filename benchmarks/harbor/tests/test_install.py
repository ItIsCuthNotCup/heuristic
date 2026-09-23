import asyncio
import hashlib
import json
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from harbor.environments.base import ExecResult

from harness_harbor.agent import APT_ARCHIVE_FALLBACK, UnrealAgent


class ScriptedEnvironment:
    """Debian bullseye image without curl whose default apt mirror has moved."""

    def __init__(self, mirror_broken: bool) -> None:
        self.commands: list[str] = []
        self.curl_installed = False
        self.mirror_broken = mirror_broken

    async def exec(
        self, command: str, user: str | None = None, **_: object
    ) -> ExecResult:
        self.commands.append(command)
        if "command -v" in command and "curl" in command:
            return ExecResult(return_code=0 if self.curl_installed else 1)
        if "for manager in apt-get" in command:
            return ExecResult(stdout="apt-get", return_code=0)
        if "archive.debian.org" in command:
            self.mirror_broken = False
            return ExecResult(return_code=0)
        if "apt-get install" in command:
            if self.mirror_broken:
                return ExecResult(
                    stderr="E: Unable to fetch some archives", return_code=100
                )
            self.curl_installed = True
            return ExecResult(return_code=0)
        raise AssertionError(f"unexpected command: {command}")


def make_agent(directory: Path) -> UnrealAgent:
    (directory / "heuristic").write_bytes(b"runner")
    (directory / "manifest.json").write_text(
        json.dumps(
            {
                "revision": "b" * 40,
                "sha256": hashlib.sha256(b"runner").hexdigest(),
                "goos": "linux",
                "goarch": "amd64",
            }
        )
    )
    return UnrealAgent(
        bundle=str(directory), logs_dir=directory, model_name="openai/test"
    )


class InstallTests(unittest.TestCase):
    def test_curl_is_installed_before_the_agent_runs(self):
        with TemporaryDirectory() as temporary:
            agent = make_agent(Path(temporary))
            environment = ScriptedEnvironment(mirror_broken=False)
            asyncio.run(agent.ensure_curl(environment))
            self.assertTrue(environment.curl_installed)
            self.assertFalse(
                any("archive.debian.org" in c for c in environment.commands)
            )

    def test_moved_apt_mirror_falls_back_to_the_archive(self):
        with TemporaryDirectory() as temporary:
            agent = make_agent(Path(temporary))
            environment = ScriptedEnvironment(mirror_broken=True)
            asyncio.run(agent.ensure_curl(environment))
            self.assertTrue(environment.curl_installed)
            installs = [
                i for i, c in enumerate(environment.commands) if "apt-get install" in c
            ]
            fallback = [
                i
                for i, c in enumerate(environment.commands)
                if APT_ARCHIVE_FALLBACK in c
            ]
            self.assertEqual(len(installs), 2)
            self.assertEqual(len(fallback), 1)
            self.assertLess(installs[0], fallback[0])
            self.assertLess(fallback[0], installs[1])


if __name__ == "__main__":
    unittest.main()

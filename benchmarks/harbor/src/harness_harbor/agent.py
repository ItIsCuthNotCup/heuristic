import json
import shlex
from pathlib import Path, PurePosixPath
from tempfile import NamedTemporaryFile
from typing import Any, override
from uuid import uuid4

import certifi
from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from harbor.models.trajectories import Agent

from harness_harbor.bundle import Bundle
from harness_harbor.trajectory import convert

APT_ARCHIVE_FALLBACK = (
    "for f in /etc/apt/sources.list $(ls /etc/apt/sources.list.d/* 2>/dev/null); do "
    "sed -i -E -e '/debian-security|security\\.debian\\.org/d' "
    "-e 's#https?://deb\\.debian\\.org/debian([ /])"
    "#http://archive.debian.org/debian\\1#g' "
    '"$f" 2>/dev/null; done; '
    "apt-get -o Acquire::Check-Valid-Until=false update"
)


class UnrealAgent(BaseInstalledAgent):
    SUPPORTS_ATIF = True
    MODEL_CONNECTION = ModelConnectionSpec()

    def __init__(
        self,
        *args: Any,
        bundle: str,
        thinking_level: str = "high",
        **kwargs: Any,
    ) -> None:
        super().__init__(*args, **kwargs)
        if thinking_level not in {"low", "medium", "high", "xhigh", "max"}:
            raise ValueError("Unsupported thinking_level")
        if not self.model_name:
            raise ValueError("Use a provider/model name, e.g. openai/gpt-5.4")
        self._provider, separator, self._model = self.model_name.partition("/")
        if (
            not separator
            or not self._model
            or self._provider not in {"openai", "openrouter", "fireworks_ai"}
        ):
            raise ValueError(
                "Model must use the openai/, openrouter/ or fireworks_ai/ prefix"
            )
        # A task may declare MCP servers or a skills directory for agents that use
        # them; this integration exposes Bash and ViewImage, so they are recorded in
        # the trajectory metadata rather than failing the trial.
        self._not_offered = {
            key: value
            for key, value in (
                ("task_mcp_servers", [server.name for server in self.mcp_servers]),
                ("task_skills_dir", self.skills_dir),
            )
            if value
        }
        self._bundle = Bundle.load(bundle)
        self._thinking_level = thinking_level
        self._runner_session = str(uuid4())
        self._remote = PurePosixPath("/installed-agent/metacog-agent")

    @staticmethod
    @override
    def name() -> str:
        return "metacog-agent"

    @override
    def version(self) -> str:
        return self._bundle.revision

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await self.ensure_curl(environment)
        await self.exec_as_root(environment, command=f"mkdir -p {self._remote}")
        # Upload a verified snapshot so replacing the local file cannot mix binaries.
        with NamedTemporaryFile() as snapshot:
            snapshot.write(self._bundle.read_binary())
            snapshot.flush()
            await environment.upload_file(
                Path(snapshot.name), str(self._remote / "runner")
            )
        await environment.upload_file(
            Path(certifi.where()), str(self._remote / "ca.pem")
        )
        await self.exec_as_root(
            environment,
            command=(
                f"chmod 755 {self._remote}/runner && "
                f"chmod 644 {self._remote}/ca.pem && "
                f"{self._remote}/runner -h"
            ),
        )

    # Terminal-Bench verifiers bootstrap their test runner with curl, and Harbor
    # installs curl for every other installed agent before the agent runs. The
    # runner itself needs nothing, but skipping this step leaves the verifier on
    # an image without curl (or with an apt mirror that has moved to the archive)
    # unable to run at all, so the trial scores zero regardless of the agent.
    async def ensure_curl(self, environment: BaseEnvironment) -> None:
        try:
            await self.ensure_system_dependencies(environment, ("curl",))
        except Exception as error:
            self.logger.warning(
                "curl install failed (%s); retrying with archive.debian.org", error
            )
            await self.exec_as_root(environment, command=APT_ARCHIVE_FALLBACK)
            await self.ensure_system_dependencies(environment, ("curl",))

    @with_prompt_template
    @override
    async def run(
        self, instruction: str, environment: BaseEnvironment, context: AgentContext
    ) -> None:
        connection = self.model_connection
        if not connection.api_key:
            raise ValueError(f"No API key configured for {self._provider}")
        logs = self.environment_logs_dir
        request = {
            "prompt": instruction,
            "model": self._model,
            "thinking_level": self._thinking_level,
            "session_id": self._runner_session,
            "disallowed_tools": ["SkillUse"],
        }
        await self.exec_as_agent(
            environment, command=f"mkdir -p {shlex.quote(str(logs))}"
        )
        await self._upload_config_text(
            environment,
            content=json.dumps(request),
            filename="request.json",
            remote_path=str(logs / "request.json"),
        )
        env = {
            "UNREAL_HARNESS_LLM_PROVIDER": (
                "fireworks" if self._provider == "fireworks_ai" else self._provider
            ),
            "UNREAL_HARNESS_LLM_API_KEY": connection.api_key,
        }
        if connection.configured_base_url:
            env["UNREAL_HARNESS_LLM_BASE_URL"] = connection.configured_base_url
        command = (
            "if [ ! -s /etc/ssl/certs/ca-certificates.crt ]; then "
            f"export SSL_CERT_FILE={self._remote}/ca.pem; fi; "
            'export SHELL="$(command -v bash)"; '
            + shlex.join(
                [
                    str(self._remote / "runner"),
                    "-workspace",
                    ".",
                    "-session-directory",
                    str(logs / "sessions"),
                    "-log-directory",
                    str(logs / "logs"),
                ]
            )
            + f" <{shlex.quote(str(logs / 'request.json'))}"
            + f" >{shlex.quote(str(logs / 'runner.jsonl'))}"
            + f" 2>{shlex.quote(str(logs / 'runner.stderr'))}"
        )
        await self.exec_as_agent(environment, command=command, env=env)

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        identity = Agent(
            name=self.name(),
            version=self.version(),
            model_name=self.model_name,
            extra={
                "binary_sha256": self._bundle.sha256,
                "arch": self._bundle.arch,
                "thinking_level": self._thinking_level,
            },
        )
        records_path = self.logs_dir / "runner.jsonl"
        if not records_path.exists():
            # The log is missing when the sandbox connection was lost before the logs
            # were downloaded. Record it rather than raise, which aborts the whole job.
            context.metadata = {
                **(context.metadata or {}),
                "agent_logs_missing": True,
                "revision": self.version(),
            }
            return
        with records_path.open() as records:
            trajectory = convert(
                records, identity, self._runner_session, output_dir=self.logs_dir
            )
        (self.logs_dir / "trajectory.json").write_text(
            json.dumps(trajectory.to_json_dict(), ensure_ascii=False, indent=2) + "\n"
        )
        metrics = trajectory.final_metrics
        context.n_input_tokens = metrics.total_prompt_tokens
        context.n_cache_tokens = metrics.total_cached_tokens
        context.n_output_tokens = metrics.total_completion_tokens
        context.cost_usd = None
        context.metadata = {
            **(context.metadata or {}),
            **identity.extra,
            **trajectory.extra,
            **self._not_offered,
            "revision": self.version(),
        }

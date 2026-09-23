import hashlib
import json
import unittest
from contextlib import ExitStack
from pathlib import Path
from tempfile import TemporaryDirectory

from harbor.models.agent.context import AgentContext
from harbor.models.task.config import MCPServerConfig

from harness_harbor.agent import UnrealAgent
from harness_harbor.bundle import Bundle


class BundleTests(unittest.TestCase):
    def test_changed_binary_is_rejected_after_construction(self):
        with TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "heuristic").write_bytes(b"original")
            (path / "manifest.json").write_text(
                json.dumps(
                    {
                        "revision": "a" * 40,
                        "sha256": hashlib.sha256(b"original").hexdigest(),
                        "goos": "linux",
                        "goarch": "amd64",
                    }
                )
            )
            bundle = Bundle.load(temporary)
            self.assertEqual(bundle.read_binary(), b"original")
            agent = UnrealAgent(
                bundle=temporary, logs_dir=path, model_name="openai/test"
            )
            self.assertEqual(agent.version(), "a" * 40)
            (path / "heuristic").write_bytes(b"replacement")
            with self.assertRaisesRegex(ValueError, "checksum"):
                bundle.read_binary()
            with self.assertRaisesRegex(ValueError, "checksum"):
                Bundle.load(temporary)

    def test_bad_trajectory_is_not_silently_accepted(self):
        with TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "heuristic").write_bytes(b"runner")
            (path / "manifest.json").write_text(
                json.dumps(
                    {
                        "revision": "a" * 40,
                        "sha256": hashlib.sha256(b"runner").hexdigest(),
                        "goos": "linux",
                        "goarch": "amd64",
                    }
                )
            )
            agent = UnrealAgent(
                bundle=temporary, logs_dir=path, model_name="openai/test"
            )
            (path / "runner.jsonl").write_text("{")
            with self.assertRaisesRegex(ValueError, "line 1"):
                agent.populate_context_post_run(AgentContext())
            self.assertFalse((path / "trajectory.json").exists())

    def test_missing_runner_log_is_recorded_not_raised(self):
        with TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "heuristic").write_bytes(b"runner")
            (path / "manifest.json").write_text(
                json.dumps(
                    {
                        "revision": "a" * 40,
                        "sha256": hashlib.sha256(b"runner").hexdigest(),
                        "goos": "linux",
                        "goarch": "amd64",
                    }
                )
            )
            agent = UnrealAgent(
                bundle=temporary, logs_dir=path, model_name="openai/test"
            )
            context = AgentContext()
            agent.populate_context_post_run(context)
            self.assertTrue(context.metadata["agent_logs_missing"])
            self.assertEqual(context.metadata["revision"], "a" * 40)
            self.assertIsNone(context.n_input_tokens)
            self.assertFalse((path / "trajectory.json").exists())

    def test_provider_keys_and_nested_model_paths(self):
        with TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "heuristic").write_bytes(b"runner")
            (path / "manifest.json").write_text(
                json.dumps(
                    {
                        "revision": "a" * 40,
                        "sha256": hashlib.sha256(b"runner").hexdigest(),
                        "goos": "linux",
                        "goarch": "amd64",
                    }
                )
            )
            for prefix, key in (
                ("openai", "OPENAI_API_KEY"),
                ("openrouter", "OPENROUTER_API_KEY"),
                ("fireworks_ai", "FIREWORKS_AI_API_KEY"),
            ):
                with self.subTest(provider=prefix):
                    agent = UnrealAgent(
                        bundle=temporary,
                        logs_dir=path,
                        model_name=f"{prefix}/organization/model",
                        extra_env={key: "test-key"},
                    )
                    self.assertEqual(agent._model, "organization/model")
                    self.assertEqual(agent.model_connection.api_key, "test-key")

    def test_invalid_model_and_reasoning_fail_before_installation(self):
        for model in ("test", "anthropic/test", "openai/"):
            with ExitStack() as stack:
                stack.enter_context(self.subTest(model=model))
                stack.enter_context(self.assertRaises(ValueError))
                UnrealAgent(bundle="missing", logs_dir=Path("."), model_name=model)
        with self.assertRaisesRegex(ValueError, "thinking_level"):
            UnrealAgent(
                bundle="missing",
                logs_dir=Path("."),
                model_name="openai/test",
                thinking_level="extreme",
            )


if __name__ == "__main__":
    unittest.main()


class TaskCapabilityTests(unittest.TestCase):
    def test_task_mcp_servers_and_skills_are_recorded_not_rejected(self):
        with TemporaryDirectory() as temporary:
            path = Path(temporary)
            (path / "heuristic").write_bytes(b"runner")
            (path / "manifest.json").write_text(
                json.dumps(
                    {
                        "revision": "b" * 40,
                        "sha256": hashlib.sha256(b"runner").hexdigest(),
                        "goos": "linux",
                        "goarch": "amd64",
                    }
                )
            )
            agent = UnrealAgent(
                bundle=temporary,
                logs_dir=path,
                model_name="openai/test",
                mcp_servers=[
                    MCPServerConfig(
                        name="playwright", transport="sse", url="http://mcp:3080/sse"
                    )
                ],
                skills_dir="/app/.agents/skills",
            )
            self.assertEqual(
                agent._not_offered,
                {
                    "task_mcp_servers": ["playwright"],
                    "task_skills_dir": "/app/.agents/skills",
                },
            )

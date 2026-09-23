import json
import unittest

from harbor.models.trajectories import Agent, Trajectory

from harness_harbor.trajectory import RUNNING, bash_result, convert


def record(sequence, kind, data):
    return json.dumps(
        {
            "Sequence": sequence,
            "Kind": kind,
            "Data": data,
            "RecordedAt": "2026-09-10T10:00:00Z",
        }
    )


def response(turn, output):
    return {
        "TurnID": turn,
        "Response": {
            "ID": turn,
            "Stop": "complete",
            "Output": output,
            "Usage": {
                "InputTokens": 10,
                "CachedInputTokens": 4,
                "CacheWriteInputTokens": 0,
                "OutputTokens": 3,
                "ReasoningTokens": 1,
            },
        },
    }


def status(state="completed", stdout="test", **fields):
    return {
        "CallID": "call-1",
        "Status": {"WaitingFor": ["op-1"]},
        "Operations": [
            {
                "ID": "op-1",
                "Type": "shell",
                "Status": state,
                "State": {
                    "Result": {"Out": stdout, "Err": "", "ExitCode": 0},
                    **fields,
                },
            }
        ],
    }


class TrajectoryTests(unittest.TestCase):
    def test_plain_text_is_not_base64_decoded(self):
        for text in ("test", "1234", "aGVsbG8=", "😃\n", "", "{not JSON", '  "hi"\n\n'):
            with self.subTest(text=text):
                self.assertEqual(
                    bash_result(status(stdout=text)), text or "(no output)"
                )

    def test_preserves_inline_truncation_without_duplicate_metadata(self):
        stdout = "head...92 bytes truncated; complete output in /out...tail"
        stderr = "head...92 bytes truncated; complete output in /err...tail"
        data = status(
            Result={"Out": stdout, "Err": stderr, "ExitCode": 7},
            OutTruncated=True,
            ErrTruncated=True,
            OutPath="/out",
            ErrPath="/err",
        )
        self.assertEqual(
            bash_result(data), f"{stdout}\nStderr:\n{stderr}\nExit code: 7"
        )

    def test_labels_stderr_and_nonzero_exit_codes(self):
        for stdout, stderr, exit_code, expected in (
            ("", "warning\n", 0, "Stderr:\nwarning\n"),
            ("out\n", "err\n", 0, "out\n\nStderr:\nerr\n"),
            ("out", "err", 7, "out\nStderr:\nerr\nExit code: 7"),
            ("", "failed\n", 7, "Stderr:\nfailed\n\nExit code: 7"),
            ("", "", 1, "Exit code: 1"),
        ):
            with self.subTest(stdout=stdout, stderr=stderr, exit_code=exit_code):
                data = status(
                    Result={"Out": stdout, "Err": stderr, "ExitCode": exit_code}
                )
                self.assertEqual(bash_result(data), expected)

    def test_running_and_failed_operations(self):
        for state in ("ready", "awaiting", "canceling"):
            with self.subTest(state=state):
                self.assertEqual(bash_result(status(state=state, Result=None)), RUNNING)
        for state in ("failed", "canceled"):
            for error in ("", "shell stopped", "err...100 bytes truncated...end"):
                with self.subTest(state=state, error=error):
                    data = status(state=state, Result=None, TerminalError=error)
                    self.assertEqual(
                        bash_result(data),
                        f"Error: {error or 'shell operation ' + state}",
                    )

    def test_failure_and_cancellation_preserve_available_captures(self):
        for state in ("failed", "canceled"):
            for fields, prefix in (
                ({}, ""),
                ({"OutPath": "/out"}, "Stdout capture: /out\n"),
                ({"ErrPath": "/err"}, "Stderr capture: /err\n"),
                (
                    {"OutPath": "/out", "ErrPath": "/err"},
                    "Stdout capture: /out\nStderr capture: /err\n",
                ),
            ):
                with self.subTest(state=state, fields=fields):
                    data = status(state=state, Result=None, **fields)
                    self.assertEqual(
                        bash_result(data), prefix + f"Error: shell operation {state}"
                    )

    def test_rejects_invalid_shell_results(self):
        for data, message in (
            ({"Status": {}}, "has 0 operations, want 1"),
            (
                {"Status": {"WaitingFor": ["op-1", "op-2"]}},
                "has 2 operations, want 1",
            ),
            (
                {**status(), "Status": {"Error": "bad arguments"}},
                "both a validation error and operations",
            ),
            (status(Result=None), "Completed shell operation has no result"),
            (status(state="unknown"), "Unsupported operation status"),
            (
                status(Result={"Out": None, "Err": "", "ExitCode": 0}),
                "Expected current runner text output",
            ),
        ):
            with self.assertRaisesRegex(ValueError, message):
                bash_result(data)

    def test_truncated_validation_error_is_plain_text(self):
        data = {
            "Status": {
                "Error": "bad...100 bytes truncated...JSON",
                "ErrorTruncated": True,
            }
        }
        self.assertEqual(bash_result(data), "Error: bad...100 bytes truncated...JSON")

    def test_internal_control_and_delayed_observations(self):
        tool_call = {
            "Type": "tool_call",
            "Data": {
                "CallID": "call-1",
                "Name": "Bash",
                "Arguments": '{"command":"printf test"}',
            },
        }
        lines = [
            record(1, "input", {"Kind": "external", "Payload": '"literal quotes"'}),
            record(2, "input", {"Kind": "control", "Payload": {"Mode": "when_idle"}}),
            record(3, "turn", {"ID": "turn-1"}),
            record(4, "model_response", response("turn-1", [tool_call])),
            record(5, "tool_call_status", status(state="ready")),
            record(6, "turn", {"ID": "turn-2"}),
            record(7, "model_response", response("turn-2", [])),
            record(8, "tool_call_status", status()),
            record(9, "turn", {"ID": "turn-3"}),
            record(10, "model_response", response("turn-3", [])),
        ]
        trajectory = convert(lines, Agent(name="heuristic", version="test"), "session")
        self.assertEqual(len(trajectory.steps), 4)
        self.assertEqual(trajectory.steps[0].message, '"literal quotes"')
        observations = trajectory.steps[1].observation.results
        self.assertEqual(observations[0].content, RUNNING)
        self.assertEqual(observations[0].extra["available_before_turn"], "turn-2")
        self.assertEqual(observations[1].extra["available_before_turn"], "turn-3")
        self.assertEqual(observations[1].content, "test")
        self.assertEqual(trajectory.final_metrics.total_prompt_tokens, 30)
        self.assertEqual(trajectory.final_metrics.total_cached_tokens, 12)
        self.assertEqual(trajectory.final_metrics.total_completion_tokens, 9)
        self.assertIsNone(trajectory.final_metrics.total_cost_usd)
        Trajectory.model_validate(trajectory.to_json_dict())

    def test_only_latest_pending_running_result_is_marked_available(self):
        for state in ("ready", "completed"):
            with self.subTest(state=state):
                calls = [
                    {
                        "Type": "tool_call",
                        "Data": {
                            "CallID": call_id,
                            "Name": "Bash",
                            "Arguments": '{"command":"printf test"}',
                        },
                    }
                    for call_id in ("call-1", "call-2")
                ]
                other_status = {**status(state="ready"), "CallID": "call-2"}
                lines = [
                    record(1, "model_response", response("turn-1", calls)),
                    record(2, "tool_call_status", status(state="ready")),
                    record(3, "tool_call_status", other_status),
                    record(4, "input", {"Kind": "external", "Payload": "continue"}),
                    record(5, "tool_call_status", status(state=state)),
                    record(6, "turn", {"ID": "turn-2"}),
                ]
                trajectory = convert(
                    lines, Agent(name="heuristic", version="test"), "session"
                )
                observations = trajectory.steps[0].observation.results
                self.assertEqual(len(observations), 3)
                self.assertEqual(observations[0].content, RUNNING)
                self.assertNotIn("available_before_turn", observations[0].extra)
                self.assertEqual(observations[1].source_call_id, "call-2")
                self.assertEqual(observations[1].content, RUNNING)
                self.assertEqual(
                    observations[1].extra["available_before_turn"], "turn-2"
                )
                self.assertEqual(
                    observations[2].content, bash_result(status(state=state))
                )
                self.assertEqual(
                    observations[2].extra["available_before_turn"], "turn-2"
                )
                self.assertEqual(observations[2].extra["sequence"], 5)

    def test_malformed_record_is_an_error_including_partial_final_line(self):
        for lines in (["{"], ['{"Kind":']):
            with self.assertRaisesRegex(ValueError, "line 1"):
                convert(lines, Agent(name="heuristic", version="test"), "session")

    def test_validation_error_and_incomplete_run_keep_usage(self):
        lines = [
            record(
                1,
                "model_response",
                response(
                    "turn-1",
                    [
                        {
                            "Type": "tool_call",
                            "Data": {
                                "CallID": "call-1",
                                "Name": "Bash",
                                "Arguments": "{",
                            },
                        }
                    ],
                ),
            ),
            record(
                2,
                "tool_call_status",
                {"CallID": "call-1", "Status": {"Error": "bad JSON"}},
            ),
            json.dumps({"type": "error", "message": "provider disconnected"}),
        ]
        trajectory = convert(lines, Agent(name="heuristic", version="test"), "session")
        self.assertEqual(trajectory.final_metrics.total_prompt_tokens, 10)
        self.assertEqual(trajectory.extra["runner_errors"], ["provider disconnected"])
        self.assertEqual(
            trajectory.steps[0].observation.results[0].content,
            "Error: bad JSON",
        )


if __name__ == "__main__":
    unittest.main()

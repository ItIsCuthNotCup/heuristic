import base64
import unittest
from contextlib import ExitStack
from pathlib import Path
from tempfile import TemporaryDirectory

from harbor.models.trajectories import Agent, Trajectory
from test_trajectory import record, response

from harness_harbor.trajectory import RUNNING, convert, view_image_result


def image_status(state="completed", **fields):
    return {
        "CallID": "image-call",
        "Status": {"WaitingFor": ["image-operation"]},
        "Operations": [
            {
                "ID": "image-operation",
                "Type": "view_image",
                "Status": state,
                "State": {
                    "Result": {
                        "Content": base64.b64encode(b"returned image bytes").decode(),
                        "OriginalMIMEType": "image/png",
                        "EncodedMIMEType": "image/png",
                        "OriginalWidth": 4000,
                        "OriginalHeight": 2000,
                        "ScaleRatio": 1,
                        "Error": "",
                        **fields,
                    }
                },
            }
        ],
    }


class ViewImageTests(unittest.TestCase):
    def test_exports_image_and_only_applicable_metadata(self):
        for original, encoded, ratio, text in (
            ("image/png", "image/png", 1, None),
            ("image/jpeg", "image/jpeg", 1, None),
            ("image/bmp", "image/png", 1, "original MIME type: image/bmp"),
            (
                "image/png",
                "image/png",
                0.5,
                "original dimensions: 4000x2000; multiply coordinates by 2.00 "
                "to approximate original",
            ),
            (
                "image/tiff",
                "image/png",
                0.25,
                "original MIME type: image/tiff; original dimensions: 4000x2000; "
                "multiply coordinates by 4.00 to approximate original",
            ),
            (
                "image/png",
                "image/png",
                1 / 3.25,
                "original dimensions: 4000x2000; multiply coordinates by 3.25 "
                "to approximate original",
            ),
            (
                "image/png",
                "image/png",
                1 / 3.456,
                "original dimensions: 4000x2000; multiply coordinates by 3.46 "
                "to approximate original",
            ),
            (
                "image/png",
                "image/png",
                1 / 120,
                "original dimensions: 4000x2000; multiply coordinates by 120.00 "
                "to approximate original",
            ),
        ):
            with ExitStack() as stack:
                stack.enter_context(self.subTest(original=original, ratio=ratio))
                directory = stack.enter_context(TemporaryDirectory())
                output_dir = Path(directory)
                data = image_status(
                    OriginalMIMEType=original,
                    EncodedMIMEType=encoded,
                    ScaleRatio=ratio,
                )
                parts = view_image_result(data, output_dir)
                self.assertEqual(parts[0].type, "image")
                self.assertEqual(parts[0].source.media_type, encoded)
                path = output_dir / parts[0].source.path
                self.assertEqual(path.parent, output_dir / "images")
                self.assertEqual(path.read_bytes(), b"returned image bytes")
                extension = ".jpg" if encoded == "image/jpeg" else ".png"
                self.assertEqual(path.suffix, extension)
                if text is None:
                    self.assertEqual(len(parts), 1)
                else:
                    self.assertEqual(len(parts), 2)
                    self.assertEqual(parts[1].type, "text")
                    self.assertEqual(parts[1].text, text)
                self.assertEqual(view_image_result(data, output_dir), parts)
                self.assertEqual(len(list(path.parent.iterdir())), 1)

    def test_running_and_errors_remain_text(self):
        for state in ("ready", "awaiting", "canceling"):
            self.assertEqual(view_image_result(image_status(state), None), RUNNING)
        for state in ("failed", "canceled"):
            with self.subTest(state=state):
                data = image_status(state, Error="exceeded MaxSize by 100 bytes")
                self.assertEqual(
                    view_image_result(data, None),
                    "Error: exceeded MaxSize by 100 bytes; "
                    "original MIME type: image/png; original dimensions: 4000x2000",
                )
                data["Operations"][0]["State"]["Result"] = None
                self.assertEqual(
                    view_image_result(data, None),
                    "Error: view-image operation " + state,
                )
        self.assertEqual(
            view_image_result({"Status": {"Error": "invalid path"}}, None),
            "Error: invalid path",
        )
        self.assertEqual(
            view_image_result(
                image_status(
                    "failed",
                    OriginalMIMEType="",
                    OriginalWidth=0,
                    OriginalHeight=0,
                    Error="decode image header; original dimensions unavailable",
                ),
                None,
            ),
            "Error: decode image header; original dimensions unavailable",
        )

    def test_rejects_invalid_results(self):
        wrong_type = image_status()
        wrong_type["Operations"][0]["Type"] = "shell"
        no_result = image_status()
        no_result["Operations"][0]["State"]["Result"] = None
        for data in (
            {"Status": {}},
            {"Status": {"WaitingFor": ["one", "two"]}},
            {**image_status(), "Status": {"Error": "invalid"}},
            wrong_type,
            no_result,
            image_status("unknown"),
            image_status(Content=""),
            image_status(Content="not base64!"),
            image_status(EncodedMIMEType="image/gif"),
            image_status(ScaleRatio=0),
            image_status(ScaleRatio=2),
            image_status(Error="failed"),
        ):
            with ExitStack() as stack:
                stack.enter_context(self.subTest(data=data))
                directory = stack.enter_context(TemporaryDirectory())
                with self.assertRaises(ValueError):
                    view_image_result(data, Path(directory))
                self.assertEqual(list(Path(directory).iterdir()), [])
        with self.assertRaisesRegex(ValueError, "require an output directory"):
            view_image_result(image_status(), None)

    def test_multimodal_observations_preserve_async_availability(self):
        call = {
            "Type": "tool_call",
            "Data": {
                "CallID": "image-call",
                "Name": "ViewImage",
                "Arguments": '{"path":"image.bmp"}',
            },
        }
        lines = [
            record(1, "model_response", response("turn-1", [call])),
            record(2, "tool_call_status", image_status("ready")),
            record(3, "turn", {"ID": "turn-2"}),
            record(4, "model_response", response("turn-2", [])),
            record(
                5,
                "tool_call_status",
                image_status(OriginalMIMEType="image/bmp", ScaleRatio=0.5),
            ),
            record(6, "turn", {"ID": "turn-3"}),
        ]
        with TemporaryDirectory() as directory:
            trajectory = convert(
                lines,
                Agent(name="heuristic", version="test"),
                "session",
                output_dir=Path(directory),
            )
            observations = trajectory.steps[0].observation.results
            self.assertEqual(observations[0].content, RUNNING)
            self.assertEqual(observations[0].extra["available_before_turn"], "turn-2")
            self.assertEqual(observations[1].extra["available_before_turn"], "turn-3")
            self.assertEqual(observations[1].source_call_id, "image-call")
            self.assertEqual(observations[1].content[0].type, "image")
            self.assertEqual(observations[1].content[1].type, "text")
            restored = Trajectory.model_validate(trajectory.to_json_dict())
            self.assertEqual(restored, trajectory)
            self.assertEqual(trajectory.final_metrics.total_prompt_tokens, 20)


if __name__ == "__main__":
    unittest.main()

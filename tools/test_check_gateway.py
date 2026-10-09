"""Synthetic checker tests; no network, GPU, customer data or credentials."""
from contextlib import contextmanager, redirect_stderr, redirect_stdout
from copy import deepcopy
import io
import json
import os
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import check_gateway as checker


SECRET = "SYNTHETIC_EXCEPTION_OR_RESPONSE_MUST_NOT_APPEAR"
TOOL_CHECKS = ("named_tool", "required_tool", "tool_roundtrip", "named_tool_stream")
USAGE = {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}


def text_result(content="Synthetic greeting"):
    return {"model": "qwen3-4b", "choices": [{"index": 0, "finish_reason": "stop",
            "message": {"role": "assistant", "content": content}}], "usage": deepcopy(USAGE)}


def tool_result():
    return {"model": "qwen3-4b", "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
        "role": "assistant", "content": None, "tool_calls": [{"id": "call_synthetic", "type": "function",
        "function": {"name": "lookup_demo", "arguments": '{"code":"TEST-001"}'}}]}}], "usage": deepcopy(USAGE)}


def chunk(delta, reason=None):
    return {"model": "qwen3-4b", "choices": [{"index": 0, "delta": delta, "finish_reason": reason}]}


def text_events():
    return [chunk({"role": "assistant", "content": "Synthetic greeting"}), chunk({}, "stop"),
            {"model": "qwen3-4b", "choices": [], "usage": deepcopy(USAGE)}]


def tool_events():
    return [chunk({"role": "assistant", "tool_calls": [{"index": 0, "id": "call_synthetic", "type": "function",
            "function": {"name": "lookup_demo", "arguments": '{"code":'}}]}),
            chunk({"tool_calls": [{"index": 0, "function": {"arguments": '"TEST-001"}'}}]}),
            chunk({}, "tool_calls"), {"model": "qwen3-4b", "choices": [], "usage": deepcopy(USAGE)}]


class FakeClient:
    def __init__(self, *, tools=True, overrides=None):
        self.overrides = overrides or {}
        self.calls = []
        self.capability = {"contract_version": "1", "tool_execution": "client", "models": [{
            "id": "qwen3-4b", "max_output_tokens": 128,
            "features": {"text": True, "streaming": True, "stream_usage": True,
                         "json_schema": True, "tool_calling": tools}}]}
        self.capabilities = SimpleNamespace(get=lambda: self.value("discovery", self.capability))
        self.models = SimpleNamespace(list=lambda: self.value("models", {"data": [{"id": "qwen3-4b"}]}))
        self.chat = SimpleNamespace(create=self.create, stream=self.stream)

    def value(self, kind, default):
        value = self.overrides.get(kind, default)
        if isinstance(value, Exception):
            raise value
        return deepcopy(value)

    def create(self, **kwargs):
        self.calls.append(kwargs)
        if "response_format" in kwargs:
            return self.value("schema", text_result('{"answer":42}'))
        if kwargs.get("tools"):
            if kwargs["tool_choice"] == "none":
                return self.value("roundtrip", text_result())
            kind = "required" if kwargs["tool_choice"] == "required" else "named"
            return self.value(kind, tool_result())
        return self.value("text", text_result())

    @contextmanager
    def stream(self, **kwargs):
        self.calls.append(kwargs)
        kind = "tool_stream" if kwargs.get("tools") else "text_stream"
        yield iter(self.value(kind, tool_events() if kwargs.get("tools") else text_events()))


class AcceptanceTests(unittest.TestCase):
    def report(self, client=None, **kwargs):
        return checker.accept(client or FakeClient(), vendor=kwargs.pop("vendor", "amd"),
                              model="qwen3-4b", **kwargs)

    def failed_check(self, kind, value, check):
        report = self.report(FakeClient(overrides={kind: value}))
        self.assertEqual(report["result"], "failed")
        self.assertEqual(report["checks"][check], "failed")
        self.assertEqual(report["hardware_acceptance"], "unverified")
        return report

    def test_both_vendors_pass_api_without_claiming_hardware(self):
        for vendor in ("amd", "nvidia"):
            with self.subTest(vendor=vendor):
                report = self.report(vendor=vendor)
                self.assertEqual(report["result"], "passed")
                self.assertEqual(report["requested_vendor"], vendor)
                self.assertEqual(set(report["checks"].values()), {"passed"})
                self.assertEqual(report["hardware_acceptance"], "unverified")
                self.assertEqual(report["gpu_identity"], "not_observed")
                for field in ("offline_isolation", "recovery", "evidence"):
                    self.assertEqual(report[field], "not_tested")

    def test_disabled_tools_are_explicitly_not_enabled(self):
        client = FakeClient(tools=False)
        report = self.report(client)
        self.assertEqual(report["result"], "passed")
        self.assertEqual({report["checks"][name] for name in TOOL_CHECKS}, {"not_enabled"})
        self.assertTrue(all("tools" not in call for call in client.calls))

    def test_required_disabled_tools_fail(self):
        report = self.report(FakeClient(tools=False), require_tools=True)
        self.assertEqual(report["result"], "failed")
        self.assertEqual({report["checks"][name] for name in TOOL_CHECKS}, {"failed"})

    def test_invalid_vendor_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "^invalid_vendor$"):
            self.report(vendor=SECRET)

    def test_invalid_discovery_stops_before_inference(self):
        for tools in (None, 1, "true"):
            with self.subTest(tools=tools):
                client = FakeClient(tools=tools)
                report = self.report(client)
                self.assertEqual(report["checks"], {"discovery": "failed"})
                self.assertEqual(client.calls, [])

    def test_missing_model_fails_discovery(self):
        client = FakeClient(overrides={"models": {"data": []}})
        self.assertEqual(self.report(client)["checks"], {"discovery": "failed"})
        self.assertEqual(client.calls, [])

    def test_text_requires_one_complete_assistant_choice(self):
        values = []
        for content in (None, "", "   ", 42):
            values.append(text_result(content))
        for field, value in (("finish_reason", "length"), ("index", 1), ("index", False)):
            result = text_result()
            result["choices"][0][field] = value
            values.append(result)
        result = text_result()
        result["choices"][0]["message"]["role"] = "user"
        values.append(result)
        result = text_result()
        result["choices"].append(deepcopy(result["choices"][0]))
        values.append(result)
        for index, result in enumerate(values):
            with self.subTest(index=index):
                self.failed_check("text", result, "text")

    def test_schema_requires_exact_duplicate_free_json(self):
        for content in ('{"answer":41}', '{"answer":"42"}', 'not-json',
                        '{"answer":42,"extra":true}', '{"answer":0,"answer":42}'):
            with self.subTest(content=content):
                self.failed_check("schema", text_result(content), "json_schema")

    def test_named_and_required_calls_validate_name_arguments_and_finish(self):
        for source, check in (("named", "named_tool"), ("required", "required_tool")):
            mutations = []
            for key, value in (("name", "unknown"), ("arguments", "not-json"),
                               ("arguments", '{"code":"wrong"}'),
                               ("arguments", '{"code":"wrong","code":"TEST-001"}')):
                response = tool_result()
                response["choices"][0]["message"]["tool_calls"][0]["function"][key] = value
                mutations.append(response)
            response = tool_result()
            response["choices"][0]["finish_reason"] = "stop"
            mutations.append(response)
            response = tool_result()
            response["choices"][0]["message"]["tool_calls"].append(
                deepcopy(response["choices"][0]["message"]["tool_calls"][0]))
            mutations.append(response)
            for index, response in enumerate(mutations):
                with self.subTest(source=source, index=index):
                    self.failed_check(source, response, check)

    def test_roundtrip_requires_completed_final_answer(self):
        self.failed_check("roundtrip", text_result(""), "tool_roundtrip")

    def test_all_reported_usage_must_be_nonnegative_integer_sum(self):
        for usage in ({"prompt_tokens": 1, "completion_tokens": -1, "total_tokens": 0},
                      {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 4},
                      {"prompt_tokens": True, "completion_tokens": 2, "total_tokens": 3},
                      {"prompt_tokens": 1, "completion_tokens": 2.0, "total_tokens": 3},
                      {"total_tokens": 3}, None):
            for source, check, factory in (("text_stream", "stream_usage", text_events),
                                           ("tool_stream", "named_tool_stream", tool_events)):
                with self.subTest(source=source, usage=usage):
                    events = factory()
                    events[-1]["usage"] = usage
                    self.failed_check(source, events, check)
            if usage is not None:
                result = text_result()
                result["usage"] = usage
                self.failed_check("text", result, "text")

    def test_stream_requires_terminal_event_order(self):
        for source, check, factory in (("text_stream", "stream_usage", text_events),
                                       ("tool_stream", "named_tool_stream", tool_events)):
            good = factory()
            cases = [good[:-1], good[:-2] + good[-1:], [good[-1]] + good[:-1],
                     good + [good[-1]], good + [chunk({"content": "late"})]]
            for index, events in enumerate(cases):
                with self.subTest(source=source, index=index):
                    self.failed_check(source, events, check)

    def test_stream_checks_tool_fragment_indexes_and_argument_completion(self):
        for index in (1, False):
            events = tool_events()
            events[0]["choices"][0]["delta"]["tool_calls"][0]["index"] = index
            self.failed_check("tool_stream", events, "named_tool_stream")
        events = tool_events()
        del events[1]
        self.failed_check("tool_stream", events, "named_tool_stream")

    def test_text_stream_rejects_tool_payload(self):
        events = text_events()
        events[0] = tool_events()[0]
        self.failed_check("text_stream", events, "stream_usage")

    def test_exception_and_response_payloads_never_enter_report_or_output(self):
        output, errors = io.StringIO(), io.StringIO()
        with redirect_stdout(output), redirect_stderr(errors):
            reports = [self.report(FakeClient(overrides={"discovery": RuntimeError(SECRET)})),
                       self.failed_check("text", RuntimeError(SECRET), "text"),
                       self.failed_check("schema", text_result(SECRET), "json_schema"),
                       self.failed_check("tool_stream", RuntimeError(SECRET), "named_tool_stream")]
        self.assertNotIn(SECRET, json.dumps(reports))
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(errors.getvalue(), "")

    def test_cli_configuration_exception_is_sanitized(self):
        output, errors = io.StringIO(), io.StringIO()
        arguments = ["check_gateway.py", "--base-url", "https://synthetic.invalid",
                     "--token-file", SECRET, "--ca-file", SECRET, "--model", SECRET, "--vendor", "nvidia"]
        with patch.object(sys, "argv", arguments), patch.object(checker, "Client", side_effect=RuntimeError(SECRET)), \
                patch.object(checker.logging, "disable"), \
                redirect_stdout(output), redirect_stderr(errors):
            code = checker.main()
        self.assertEqual(code, 1)
        report = json.loads(output.getvalue())
        self.assertEqual(report["checks"], {"configuration": "failed"})
        self.assertEqual(report["hardware_acceptance"], "unverified")
        self.assertNotIn(SECRET, output.getvalue() + errors.getvalue())
        self.assertEqual(errors.getvalue(), "")

    def test_optimized_python_keeps_accept_validation_active(self):
        # Import accept directly under -O: a guard limited to __main__ would
        # leave this embedding path vulnerable to removed assert statements.
        script = """import json
from test_check_gateway import FakeClient, text_result
from check_gateway import accept
good = accept(FakeClient(), vendor='amd', model='qwen3-4b')
bad = accept(FakeClient(overrides={'text': text_result('')}), vendor='nvidia', model='qwen3-4b')
print(json.dumps([good['result'], bad['result'], bad['checks']['text']]))
"""
        env = os.environ.copy()
        env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1] / "sdk/python/src")
        result = subprocess.run([sys.executable, "-O", "-c", script], cwd=Path(__file__).parent,
                                env=env, capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, "optimized checker subprocess failed")
        self.assertEqual(json.loads(result.stdout), ["passed", "failed", "failed"])
        self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()

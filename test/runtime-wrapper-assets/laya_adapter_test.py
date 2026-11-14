#!/usr/bin/env python3
"""The Laya adapter against a fake laya.load with laya 0.4.31's behaviour."""

import importlib.util
import json
import os
import pathlib
import tempfile
import unittest

HERE = pathlib.Path(__file__).parent
QUESTIONS = {
    "refund": {"type": "noul", "instructions": "Does the customer ask for a refund?"},
    "topic": {"type": "choice", "instructions": "What is it about?", "criteria": {"billing": "Money", "shipping": "Parcels"}},
    "urgency": {"type": "score", "instructions": "How urgent?", "criteria": ["Low", "High"]},
}


def load(name):
    spec = importlib.util.spec_from_file_location(name, HERE / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FakeAgent:
    """laya.agent.Agent as far as the adapter sees it.

    Like laya 0.4.31: loading rewrites tokenizer/tokenizer_config.json in place
    when it stores extra_special_tokens as a list, and falls back to the CPU
    with only a warning when CUDA is unusable; predict validates each question
    with laya.agent's ValueError messages, answers {} with empty answers,
    truncates a long state instead of refusing it (usage.truncated), and
    returns the full payload (routing, action heads, answer_confidence,
    truncation facts).
    """

    MAX_LEN = 16  # state tokens the checkpoint reads; one token per word here

    def __init__(self, path, device, fail_predict=None, cuda_usable=True):
        self.path, self.calls, self.fail_predict = pathlib.Path(path), [], fail_predict
        self.device = device if cuda_usable or not device.startswith("cuda") else "cpu"
        config = self.path / "tokenizer" / "tokenizer_config.json"
        data = json.loads(config.read_text())
        if isinstance(data.get("extra_special_tokens"), list):
            data["extra_special_tokens"] = {f"extra_{i}": t for i, t in enumerate(data["extra_special_tokens"])}
            tmp = config.with_name(".tokenizer_config.tmp")
            tmp.write_text(json.dumps(data))
            os.replace(tmp, config)
        for name in ("rl_agent_config.json", "model.safetensors"):
            (self.path / name).read_bytes()

    def predict(self, state, questions):
        self.calls.append((state, questions))
        if self.fail_predict:
            raise self.fail_predict
        answers = {}
        for name, question in questions.items():
            self.validate(name, question)
            kind = question["type"]
            extra = {"answer_confidence": 0.9, "action": {"act_probability": 1.0}}
            if kind == "noul":
                answers[name] = {"type": "noul", "noul": 0.9, "confidence": 0.9, **extra}
            elif kind == "choice":
                labels = list(question["criteria"])
                probs = {label: (0.8 if i == 0 else 0.2 / max(len(labels) - 1, 1)) for i, label in enumerate(labels)}
                answers[name] = {"type": "choice", "choice": labels[0], "confidence": 0.7, "probabilities": probs, **extra}
            else:
                levels = question["criteria"]
                answers[name] = {"type": "score", "score": 0.5, "confidence": 0.1,
                                 "probabilities": {str(i): 1 / len(levels) for i in range(len(levels))},
                                 "legend": {str(i): level for i, level in enumerate(levels)}, **extra}
        words = len(str(state).split())
        dropped = max(words - self.MAX_LEN, 0)
        tokens = 40 + words - dropped if answers else 0
        return {"model": "laya-rl-agent", "answers": answers,
                "routing": {"model": "english", "reason": "explicit"},
                "usage": {"input_tokens": tokens, "output_tokens": 0, "state_tokens": words,
                          "state_tokens_dropped": dropped, "truncated": dropped > 0,
                          "truncated_questions": list(questions) if dropped else []}}

    @staticmethod
    def validate(name, question):
        # laya/agent.py's checks, in its order and with its messages.
        if not isinstance(question, dict):
            raise ValueError(f"question {name!r}: definition must be a dict, got {type(question).__name__}")
        kind = question.get("type")
        if not isinstance(kind, str) or kind not in ("choice", "noul", "score"):
            raise ValueError(f"question {name!r}: unknown type {kind!r}; use one of ['choice', 'noul', 'score']")
        if "instructions" not in question:
            raise ValueError(f"question {name!r}: no 'instructions'; add the text the model should answer")
        instructions = question["instructions"]
        if instructions is None:
            raise ValueError(f"question {name!r}: 'instructions' must not be None; add the text the model should answer")
        if isinstance(instructions, (str, list, dict)) and not (instructions.strip() if isinstance(instructions, str) else instructions):
            raise ValueError(f"question {name!r}: 'instructions' must not be empty; add the text the model should answer")
        criteria = question.get("criteria")
        if kind == "choice":
            if not isinstance(criteria, (dict, list)):
                raise ValueError(f"question {name!r}: a choice question takes 'criteria' as a dict of "
                                 "label -> description, or a list of labels")
            if not criteria:
                raise ValueError(f"question {name!r}: a choice question needs at least one criterion")
        elif kind == "score":
            if not isinstance(criteria, list):
                raise ValueError(f"question {name!r}: a score question takes 'criteria' as a list of level "
                                 "descriptions, index 0 first")
            if not criteria:
                raise ValueError(f"question {name!r}: a score question needs at least one level")
        elif criteria is not None and not isinstance(criteria, dict):
            raise ValueError(f"question {name!r}: a noul question takes 'criteria' as a dict with optional "
                             "'true'/'false' descriptions, or omits it")


class LayaAdapterTest(unittest.TestCase):
    def setUp(self):
        self.module = load("laya_adapter")
        self.model = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.model, ignore_errors=True))
        (self.model / "tokenizer").mkdir()
        (self.model / "encoder").mkdir()
        (self.model / "encoder" / "config.json").write_text("{}")
        (self.model / "rl_agent_config.json").write_text('{"max_len": 1024}')
        (self.model / "model.safetensors").write_bytes(b"weights")
        self.tokenizer_config = '{"extra_special_tokens": ["<start_of_turn>", "<end_of_turn>"]}'
        (self.model / "tokenizer" / "tokenizer_config.json").write_text(self.tokenizer_config)
        (self.model / "tokenizer" / "tokenizer.json").write_text("{}")
        self.agents = []

    def loader(self, path, device):
        agent = FakeAgent(path, device)
        self.agents.append(agent)
        return agent

    def create(self, **config):
        adapter = self.module.LayaAdapter(str(self.model), config, loader=self.loader)
        self.addCleanup(adapter.close)
        return adapter

    def test_loads_through_a_private_directory_and_leaves_the_weights_untouched(self):
        for path in self.model.rglob("*"):
            path.chmod(0o555 if path.is_dir() else 0o444)
        self.addCleanup(lambda: [p.chmod(0o755) for p in self.model.rglob("*") if p.is_dir()])
        self.model.chmod(0o555)
        self.addCleanup(self.model.chmod, 0o755)

        adapter = self.create(device="cuda:0")
        agent = self.agents[0]
        self.assertEqual(agent.device, "cuda:0")
        self.assertNotEqual(agent.path, self.model)
        self.assertTrue(agent.path.is_absolute())
        self.assertEqual((agent.path / "model.safetensors").resolve(), (self.model / "model.safetensors").resolve())
        self.assertEqual((self.model / "tokenizer" / "tokenizer_config.json").read_text(), self.tokenizer_config)
        self.assertIn('"extra_0"', (agent.path / "tokenizer" / "tokenizer_config.json").read_text())
        # Warmed up before /health reports ready.
        self.assertEqual(agent.calls, [("1", {"q": {"type": "noul", "instructions": "Is the state a number?"}})])
        workdir = adapter.workdir
        adapter.close()
        self.assertFalse(workdir.exists())

    def test_device_defaults_to_cuda(self):
        self.create()
        self.assertEqual(self.agents[0].device, "cuda")

    def test_refuses_a_directory_that_is_not_a_laya_checkpoint(self):
        (self.model / "rl_agent_config.json").unlink()
        with self.assertRaisesRegex(FileNotFoundError, "rl_agent_config.json"):
            self.module.LayaAdapter(str(self.model), {}, loader=self.loader)

    def test_a_failed_load_or_warmup_removes_the_private_directory(self):
        made = []
        real_mkdtemp = self.module.tempfile.mkdtemp
        self.module.tempfile.mkdtemp = lambda **kw: made.append(real_mkdtemp(**kw)) or made[-1]
        self.addCleanup(setattr, self.module.tempfile, "mkdtemp", real_mkdtemp)

        def broken(path, device):
            return FakeAgent(path, device, fail_predict=RuntimeError("CUDA out of memory"))

        with self.assertRaisesRegex(RuntimeError, "out of memory"):
            self.module.LayaAdapter(str(self.model), {}, loader=broken)
        __import__("shutil").rmtree(self.model / "tokenizer")
        with self.assertRaisesRegex(FileNotFoundError, "tokenizer"):
            self.module.LayaAdapter(str(self.model), {}, loader=self.loader)
        self.assertEqual(len(made), 2)
        self.assertFalse(any(os.path.exists(path) for path in made))

    def test_answers_with_the_strict_system_one_response(self):
        adapter = self.create()
        result = adapter.invoke("systemone.decide", {"model": "triage", "state": "My parcel broke", "questions": QUESTIONS}, {})
        self.assertEqual(result["content_type"], "application/json")
        self.assertEqual(result["body"], {
            "model": "triage",
            "answers": {
                "refund": {"type": "noul", "noul": 0.9},
                "topic": {"type": "choice", "choice": "billing", "confidence": 0.7,
                          "probabilities": {"billing": 0.8, "shipping": 0.2}},
                "urgency": {"type": "score", "score": 0.5, "confidence": 0.1,
                            "probabilities": {"0": 0.5, "1": 0.5}, "legend": {"0": "Low", "1": "High"}},
            },
            "usage": {"input_tokens": 43, "output_tokens": 0},
        })
        self.assertEqual(self.agents[0].calls[-1], ("My parcel broke", QUESTIONS))
        # Without a model name the checkpoint's own is kept; JSON states pass through.
        body = adapter.invoke("systemone.decide", {"state": {"ticket": [1, 2]}, "questions": QUESTIONS}, {})["body"]
        self.assertEqual(body["model"], "laya-rl-agent")
        self.assertEqual(self.agents[0].calls[-1][0], {"ticket": [1, 2]})

    def test_question_errors_from_laya_are_the_clients(self):
        adapter = self.create()
        for questions, message in (
            ({"q": {"type": "bogus", "instructions": "?"}}, "unknown type"),
            ({"q": {"type": [], "instructions": "?"}}, "unknown type"),
            ({"q": "str"}, "must be a dict"),
            ({"q": {"type": "noul"}}, "no 'instructions'"),
            ({"q": {"type": "noul", "instructions": "  "}}, "must not be empty"),
            ({"q": {"type": "choice", "instructions": "?"}}, "takes 'criteria'"),
            ({"q": {"type": "choice", "instructions": "?", "criteria": {}}}, "at least one criterion"),
            ({"q": {"type": "score", "instructions": "?", "criteria": []}}, "at least one level"),
            ({"q": {"type": "noul", "instructions": "?", "criteria": ["a"]}}, "noul question takes"),
        ):
            with self.subTest(message), self.assertRaisesRegex(self.module.RequestError, message) as caught:
                adapter.invoke("systemone.decide", {"state": "x", "questions": questions}, {})
            self.assertEqual(caught.exception.http_status, 400)
            self.assertEqual(caught.exception.code, "invalid_request")

    def test_engine_faults_stay_server_errors(self):
        adapter = self.create()
        self.agents[0].fail_predict = RuntimeError("CUDA error: device-side assert")
        with self.assertRaises(RuntimeError) as caught:
            adapter.invoke("systemone.decide", {"state": "x", "questions": QUESTIONS}, {})
        self.assertNotIsInstance(caught.exception, self.module.RequestError)

    def test_request_limits_match_laya_serve(self):
        adapter = self.create()
        calls = len(self.agents[0].calls)
        choice = lambda n: {"type": "choice", "instructions": "?", "criteria": {f"o{i}": "x" for i in range(n)}}
        score = lambda n: {"type": "score", "instructions": "?", "criteria": ["x"] * n}
        cases = (
            ({"questions": QUESTIONS}, "state is required", "invalid_request"),
            ({"state": None, "questions": QUESTIONS}, "state is required", "invalid_request"),
            ({"state": "x", "questions": []}, "must be an object", "invalid_request"),
            ({"state": "x", "questions": {}}, "at least one question", "invalid_request"),
            ({"state": "x", "questions": {f"q{i}": QUESTIONS["refund"] for i in range(65)}}, "too many questions", "invalid_request"),
            ({"state": "x", "questions": {"c": choice(101)}}, "too many choice options", "invalid_request"),
            ({"state": "x", "questions": {"s": score(33)}}, "too many score levels", "invalid_request"),
            ({"state": "x", "questions": {"s": {"type": "score", "instructions": "?", "criteria": ["a", None]}}}, "null level", "invalid_request"),
            ({"state": "x", "questions": {f"c{i}": choice(100) for i in range(6)}}, "across questions", "invalid_request"),
            ({"state": "x" * 50_001, "questions": QUESTIONS}, "longer than 50000", "context_length_exceeded"),
            ({"state": {"k": "x" * 50_000}, "questions": QUESTIONS}, "longer than 50000", "context_length_exceeded"),
            ({"state": "bad \ud800", "questions": QUESTIONS}, "surrogate", "invalid_request"),
            ({"state": "x", "questions": {"q": {"type": "noul", "instructions": "\udfff"}}}, "surrogate", "invalid_request"),
            ({"state": "x", "questions": QUESTIONS, "images": ["data:image/png;base64,AAAA"]}, "text only", "invalid_request"),
        )
        for payload, message, code in cases:
            with self.subTest(message), self.assertRaisesRegex(self.module.RequestError, message) as caught:
                adapter.invoke("systemone.decide", payload, {})
            self.assertEqual((caught.exception.http_status, caught.exception.code), (400, code))
        self.assertEqual(len(self.agents[0].calls), calls)
        # At the limits, and with an empty image list, the request goes through.
        ok = {"state": "x" * 50_000, "questions": {"c": choice(100), "s": score(32)}, "images": []}
        adapter.invoke("systemone.decide", ok, {})
        self.assertEqual(len(self.agents[0].calls), calls + 1)

    def test_a_gpu_laya_cannot_use_fails_the_load(self):
        def no_cuda(path, device):
            return FakeAgent(path, device, cuda_usable=False)

        with self.assertRaisesRegex(RuntimeError, "loaded on cpu, not cuda"):
            self.module.LayaAdapter(str(self.model), {}, loader=no_cuda)
        # A template that asks for the CPU gets it.
        adapter = self.module.LayaAdapter(str(self.model), {"device": "cpu"}, loader=no_cuda)
        self.addCleanup(adapter.close)
        self.assertEqual(adapter.agent.device, "cpu")

    def test_a_truncated_state_is_answered_and_logged(self):
        adapter = self.create()
        with self.assertLogs("gpuworks.laya", "WARNING") as logs:
            body = adapter.invoke("systemone.decide", {"state": "word " * 20, "questions": QUESTIONS}, {})["body"]
        self.assertEqual(body["usage"], {"input_tokens": 56, "output_tokens": 0})
        self.assertIn("4 of 20 tokens dropped", logs.output[0])

    def test_a_deeply_nested_state_is_the_clients_fault(self):
        adapter = self.create()
        state = []
        for _ in range(5000):
            state = [state]
        with self.assertRaisesRegex(self.module.RequestError, "nested too deeply"):
            adapter.invoke("systemone.decide", {"state": state, "questions": QUESTIONS}, {})

    def test_rejects_other_operations(self):
        adapter = self.create()
        with self.assertRaisesRegex(ValueError, "does not implement"):
            adapter.invoke("chat.completions", {}, {})

    def test_catalog_bundles_contain_tested_sources(self):
        import tarfile

        models = HERE.parents[1] / "internal/server/inferencecatalog/seed/catalog/models/convaiinnovations"
        for name in ("laya", "laya-multilingual", "laya-typed-decisions"):
            bundle = models / name / "assets/gpuworks-laya-runtime.tar.gz"
            with self.subTest(name), tarfile.open(bundle) as archive:
                self.assertEqual(sorted(archive.getnames()), ["gpuworks_serve.py", "laya_adapter.py"])
                for member in archive.getnames():
                    self.assertEqual(archive.extractfile(member).read(), (HERE / member).read_bytes())


if __name__ == "__main__":
    unittest.main()

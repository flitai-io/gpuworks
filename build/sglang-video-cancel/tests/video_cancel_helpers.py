# SPDX-License-Identifier: Apache-2.0
# Backported from SGLang PR 47055, commit 8b0f2f929f05a4eb447fe143ecd3ba54e688f33a.
import time
import uuid
import pytest
from sglang.multimodal_gen.configs.sample.sampling_params import SamplingParams
from sglang.multimodal_gen.runtime.entrypoints.openai import video_cancel
from sglang.multimodal_gen.runtime.managers import request_cancel
from sglang.multimodal_gen.runtime.pipelines_core.schedule_batch import Req


def _req(request_id="job", cancellable=True, is_warmup=False):
    req = Req(
        sampling_params=SamplingParams(prompt="p", seed=1, request_id=request_id),
        is_warmup=is_warmup,
    )
    req.cancellable = cancellable
    return req


class _FakeListener:
    def __init__(self, cancelled_ids):
        self.cancelled_ids = set(cancelled_ids)

    def is_cancelled(self, request_ids):
        return bool(request_ids) and all(
            request_id in self.cancelled_ids for request_id in request_ids
        )


@pytest.fixture
def cancel_state(monkeypatch):
    """Enable the checks on a single-rank replica with a fake listener."""

    def set_cancelled(*request_ids):
        monkeypatch.setattr(request_cancel, "_listener", _FakeListener(request_ids))

    monkeypatch.setattr(request_cancel, "_enabled", True)
    monkeypatch.setattr(request_cancel, "model_parallel_is_initialized", lambda: False)
    set_cancelled()
    return set_cancelled


def _cancel_endpoint():
    return f"ipc:///tmp/sglang-cancel-test-{uuid.uuid4().hex[:8]}"


def _deliver_cancel(publish, listener, request_id="job"):
    for _ in range(500):
        try:
            publish(request_id)
        except video_cancel.CancelDeliveryError:
            pass
        if listener.is_cancelled([request_id]):
            return
        time.sleep(0.01)
    pytest.fail("Cancellation did not reach the listener")

# SPDX-License-Identifier: Apache-2.0
# Backported from SGLang PR 47055, commit 8b0f2f929f05a4eb447fe143ecd3ba54e688f33a.
import asyncio
from types import SimpleNamespace
from unittest.mock import Mock
import pytest
import zmq
from fastapi import HTTPException
from sglang.multimodal_gen.configs.sample.sampling_params import SamplingParams
from sglang.multimodal_gen.runtime.entrypoints import http_server
from sglang.multimodal_gen.runtime import scheduler_client as scheduler_client_module
from sglang.multimodal_gen.runtime.entrypoints.openai import video_api, video_cancel
from sglang.multimodal_gen.runtime.entrypoints.openai.stores import AsyncDictStore
from sglang.multimodal_gen.runtime.managers.request_cancel import CancelListener
from sglang.multimodal_gen.runtime.pipelines_core.schedule_batch import Req
from sglang.multimodal_gen.runtime.scheduler_client import AsyncSchedulerClient
from video_cancel_helpers import _cancel_endpoint, _deliver_cancel


_VIDEO_JOB = {
    "id": "job",
    "object": "video",
    "model": "m",
    "status": "in_progress",
    "progress": 40,
    "created_at": 0,
    "size": "1x1",
    "seconds": "1",
}


def test_delete_publishes_cancel(monkeypatch):
    store = AsyncDictStore()
    published = []
    monkeypatch.setattr(video_api, "VIDEO_STORE", store)
    monkeypatch.setattr(video_api, "publish_cancel", published.append)

    async def run_test():
        await store.upsert("job", dict(_VIDEO_JOB))
        response = await video_api.delete_video("job")
        assert response.status == "deleted"
        assert await store.get("job") is None
        with pytest.raises(HTTPException):
            await video_api.delete_video("job")

    asyncio.run(run_test())
    assert published == ["job"]


def test_delete_preserves_job_when_cancel_needs_retry(monkeypatch):
    store = AsyncDictStore()
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint)
    context = zmq.Context()
    monkeypatch.setattr(video_api, "VIDEO_STORE", store)
    monkeypatch.setattr(video_api, "publish_cancel", publisher.publish)

    async def run_test():
        await store.upsert("job", dict(_VIDEO_JOB))
        with pytest.raises(HTTPException) as error:
            await video_api.delete_video("job")
        assert error.value.status_code == 503
        assert error.value.headers == {"Retry-After": "1"}
        assert (await store.get("job"))["status"] == "in_progress"
        listener = CancelListener(context, endpoint)
        _deliver_cancel(publisher.publish, listener, "ready")
        assert (await video_api.delete_video("job")).status == "deleted"
        assert await store.get("job") is None
        for _ in range(500):
            if listener.is_cancelled(["job"]):
                break
            await asyncio.sleep(0.01)
        assert listener.is_cancelled(["job"])

    try:
        asyncio.run(run_test())
    finally:
        context.destroy(linger=0)
        publisher.close()


def test_delete_completed_job_needs_no_cancel_channel(monkeypatch):
    store = AsyncDictStore()
    publish = Mock(side_effect=video_cancel.CancelDeliveryError("unavailable"))
    monkeypatch.setattr(video_api, "VIDEO_STORE", store)
    monkeypatch.setattr(video_api, "publish_cancel", publish)

    async def run_test():
        await store.upsert("job", {**_VIDEO_JOB, "status": "completed"})
        assert (await video_api.delete_video("job")).status == "deleted"

    asyncio.run(run_test())
    publish.assert_not_called()


def test_delete_after_rpc_timeout_publishes_cancel(monkeypatch):
    """A timeout marks the job failed, but the GPU still runs it."""
    store = AsyncDictStore()
    published = []
    monkeypatch.setattr(video_api, "VIDEO_STORE", store)
    monkeypatch.setattr(video_api, "publish_cancel", published.append)

    async def timed_out(*args, **kwargs):
        raise TimeoutError("Scheduler did not respond in time.")

    monkeypatch.setattr(video_api, "process_generation_batch", timed_out)

    async def run_test():
        await store.upsert("job", dict(_VIDEO_JOB))
        task = video_api._start_video_job(
            "job",
            video_api._dispatch_job_async(
                "job",
                Req(sampling_params=SamplingParams(prompt="p")),
                temp_dirs=[],
            ),
        )
        await task
        assert (await store.get("job"))["status"] == "failed"
        await video_api.delete_video("job")

    asyncio.run(run_test())
    assert published == ["job"]


def test_stopped_job_of_deleted_video_is_not_failed(monkeypatch):
    store = AsyncDictStore()
    monkeypatch.setattr(video_api, "VIDEO_STORE", store)

    async def stopped(*args, **kwargs):
        # DELETE removed the job while it ran
        await store.pop("job")
        raise RuntimeError("Request job was cancelled")

    monkeypatch.setattr(video_api, "process_generation_batch", stopped)
    exception = Mock()
    monkeypatch.setattr(video_api.logger, "exception", exception)

    async def run_test():
        await store.upsert("job", {"status": "in_progress", "progress": 40})
        await video_api._dispatch_job_async(
            "job",
            Req(sampling_params=SamplingParams(prompt="test")),
            temp_dirs=[],
        )
        assert await store.get("job") is None

    asyncio.run(run_test())
    exception.assert_not_called()


def test_lifespan_starts_and_stops_the_cancel_publisher(monkeypatch):
    started = Mock()
    stopped = Mock()
    monkeypatch.setattr(http_server, "start_cancel_publisher", started)
    monkeypatch.setattr(http_server, "stop_cancel_publisher", stopped)
    with zmq.Context() as context, context.socket(zmq.REP) as probe:
        broker_port = probe.bind_to_random_port("tcp://127.0.0.1")
        probe.unbind(probe.last_endpoint)
    args = SimpleNamespace(
        enable_metrics=False,
        warmup_mode="off",
        progress_endpoint=None,
        cancel_endpoint="ipc:///tmp/sglang-cancel-lifespan",
        dp_size=2,
        broker_port=broker_port,
        prompt_enhancer_config=None,
        log_requests=False,
        log_requests_level=0,
        log_requests_format="text",
        log_requests_target=None,
        pipeline_config=Mock(
            supports_action_endpoint=lambda: False,
            supports_openpi_endpoint=lambda: False,
        ),
    )

    async def run_test():
        client = AsyncSchedulerClient()
        monkeypatch.setattr(scheduler_client_module, "async_scheduler_client", client)
        app = http_server.create_app(args)
        async with http_server.lifespan(app):
            started.assert_called_once_with("ipc:///tmp/sglang-cancel-lifespan", 2)
            stopped.assert_not_called()
        stopped.assert_called_once()

    asyncio.run(run_test())

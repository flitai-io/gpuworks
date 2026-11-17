# SPDX-License-Identifier: Apache-2.0
# Backported from SGLang PR 47055, commit 8b0f2f929f05a4eb447fe143ecd3ba54e688f33a.
import json
import os
import subprocess
import sys
import threading
import time
from types import SimpleNamespace
from unittest.mock import Mock
import pytest
import torch.distributed as dist
import torch.multiprocessing as mp
import zmq
from sglang.multimodal_gen.runtime.disaggregation.roles import RoleType
from sglang.multimodal_gen.runtime.entrypoints.openai import video_cancel
from sglang.multimodal_gen.runtime.managers import request_cancel
from sglang.multimodal_gen.runtime.managers.request_cancel import CancelListener, RequestCancelledError, raise_if_cancelled
from sglang.multimodal_gen.runtime.managers.scheduler import Scheduler
from sglang.multimodal_gen.runtime.server_args import ServerArgs
from video_cancel_helpers import _FakeListener, _cancel_endpoint, _deliver_cancel, _req, cancel_state


def test_published_cancel_reaches_listener():
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint)
    context = zmq.Context()
    listener = CancelListener(context, endpoint)
    try:
        _deliver_cancel(publisher.publish, listener)
        assert listener.is_cancelled(["job"])
        assert listener.is_cancelled(["job:0", "job:1"])
        assert not listener.is_cancelled(["other"])
        assert not listener.is_cancelled(["job", "other"])
        assert not listener.is_cancelled([])
    finally:
        context.destroy(linger=0)
        publisher.close()
    assert not os.path.exists(endpoint.removeprefix("ipc://"))


def test_listener_forgets_oldest_ids(monkeypatch):
    monkeypatch.setattr(request_cancel, "_MAX_CANCELLED_IDS", 2)
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint)
    context = zmq.Context()
    listener = CancelListener(context, endpoint)
    try:
        _deliver_cancel(publisher.publish, listener, "probe")
        for request_id in ("a", "b"):
            publisher.publish(request_id)
        for _ in range(500):
            if listener.is_cancelled(["b"]):
                break
            time.sleep(0.01)
        assert listener.is_cancelled(["a", "b"])
        assert not listener.is_cancelled(["probe"])
    finally:
        context.destroy(linger=0)
        publisher.close()


def test_publish_without_publisher_is_a_no_op(monkeypatch):
    monkeypatch.setattr(video_cancel, "_publisher", None)
    video_cancel.publish_cancel("job")


@pytest.mark.parametrize(
    "module",
    [
        "sglang.multimodal_gen.runtime.managers.request_cancel",
        "sglang.multimodal_gen.runtime.managers.request_cancel_error",
        "sglang.multimodal_gen.runtime.utils.logging_utils",
        "sglang.multimodal_gen.runtime.utils.perf_logger",
        "sglang.multimodal_gen.runtime.entrypoints.openai.video_cancel",
        "sglang.multimodal_gen.runtime.pipelines_core.executors.pipeline_executor",
    ],
)
def test_module_imports_first(module):
    """The stages import request_cancel, so it must not import the stages."""
    subprocess.run([sys.executable, "-c", f"import {module}"], check=True)


def test_raise_if_cancelled(cancel_state):
    raise_if_cancelled([_req()])
    cancel_state("job")
    with pytest.raises(RequestCancelledError, match="job"):
        raise_if_cancelled([_req()])


@pytest.mark.parametrize(
    "reqs",
    [
        [],
        [_req(cancellable=False)],
        [_req(is_warmup=True)],
        # a group stops only when each of its requests is cancelled
        [_req(), _req("other")],
    ],
)
def test_raise_if_cancelled_skips(cancel_state, reqs):
    cancel_state("job")
    raise_if_cancelled(reqs)


def test_raise_if_cancelled_is_disabled_without_endpoint(cancel_state, monkeypatch):
    cancel_state("job")
    monkeypatch.setattr(request_cancel, "_enabled", False)
    raise_if_cancelled([_req()])
    assert request_cancel.step_cancel_check(_req()) is None


def _agree_on_cancel(rank, init_file, driver_cancelled, result_dir):
    dist.init_process_group(
        "gloo", init_method=f"file://{init_file}", rank=rank, world_size=2
    )
    try:
        request_cancel._enabled = True
        # only the driver reads the cancelled ids; rank 1 answers the opposite
        # to show that its own view does not count
        cancelled = driver_cancelled if rank == 0 else not driver_cancelled
        request_cancel._listener = _FakeListener(["job"] if cancelled else [])
        request_cancel.model_parallel_is_initialized = lambda: True
        request_cancel.get_replica_group = lambda: SimpleNamespace(
            world_size=2, ranks=[0, 1], cpu_group=dist.group.WORLD
        )
        try:
            raise_if_cancelled([_req()])
            stopped = False
        except RequestCancelledError:
            stopped = True
        with open(os.path.join(result_dir, str(rank)), "w") as f:
            json.dump(stopped, f)
    finally:
        dist.destroy_process_group()


@pytest.mark.parametrize("driver_cancelled", [False, True])
def test_replica_ranks_follow_the_driver(tmp_path, driver_cancelled):
    """Each rank must stop at the same step, or the others wait in a collective."""
    mp.spawn(
        _agree_on_cancel,
        args=(str(tmp_path / "init"), driver_cancelled, str(tmp_path)),
        nprocs=2,
    )
    results = [json.loads((tmp_path / str(rank)).read_text()) for rank in (0, 1)]
    assert results == [driver_cancelled, driver_cancelled]


@pytest.mark.parametrize(
    "port, role, expected",
    [
        (None, RoleType.MONOLITHIC, None),
        (30000, RoleType.DENOISER, None),
        (30000, RoleType.MONOLITHIC, "sglang-diffusion-cancel-30000"),
    ],
)
def test_cancel_endpoint(port, role, expected):
    endpoint = ServerArgs.cancel_endpoint.fget(
        SimpleNamespace(port=port, disagg_role=role)
    )
    if expected is None:
        assert endpoint is None
    else:
        assert endpoint.startswith("ipc://") and endpoint.endswith(expected)


def test_init_request_cancel(monkeypatch):
    monkeypatch.setattr(request_cancel, "_enabled", False)
    monkeypatch.setattr(request_cancel, "_listener", None)
    # a rank that is not the driver takes part in each decision without a socket
    request_cancel.init_request_cancel(_cancel_endpoint(), context=None)
    assert request_cancel._enabled and request_cancel._listener is None
    context = zmq.Context()
    try:
        request_cancel.init_request_cancel(_cancel_endpoint(), context=context)
        assert isinstance(request_cancel._listener, CancelListener)
    finally:
        context.destroy(linger=0)


def test_init_request_cancel_survives_a_failed_connect(monkeypatch):
    monkeypatch.setattr(request_cancel, "_enabled", False)
    monkeypatch.setattr(request_cancel, "_listener", None)
    context = zmq.Context()
    try:
        request_cancel.init_request_cancel("bogus://endpoint", context=context)
    finally:
        context.destroy(linger=0)
    assert request_cancel._enabled and request_cancel._listener is None


def test_cancel_publisher_start_and_stop(monkeypatch):
    monkeypatch.setattr(video_cancel, "_publisher", None)
    endpoint = _cancel_endpoint()
    video_cancel.start_cancel_publisher(endpoint)
    context = zmq.Context()
    listener = CancelListener(context, endpoint)
    try:
        _deliver_cancel(video_cancel.publish_cancel, listener)
        assert listener.is_cancelled(["job"])
    finally:
        context.destroy(linger=0)
        video_cancel.stop_cancel_publisher()
    assert video_cancel._publisher is None
    assert not os.path.exists(endpoint.removeprefix("ipc://"))
    video_cancel.stop_cancel_publisher()


def test_cancel_publisher_failures(monkeypatch):
    monkeypatch.setattr(video_cancel, "_publisher", None)
    with pytest.raises(zmq.ZMQError):
        video_cancel.start_cancel_publisher("bogus://endpoint")
    assert video_cancel._publisher is None

    publisher = video_cancel.CancelPublisher(_cancel_endpoint())
    try:
        with pytest.raises(video_cancel.CancelDeliveryError, match="connected"):
            publisher.publish("job")
        publisher._socket.close()
        with pytest.raises(video_cancel.CancelDeliveryError):
            publisher.publish("job")
    finally:
        publisher.close()


def test_cancel_backpressure_does_not_drop_accepted_commands():
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint)
    context = zmq.Context()
    listener = CancelListener(context, endpoint)
    try:
        _deliver_cancel(publisher.publish, listener, "ready")
        accepted = set()
        rejected = set()
        for index in range(3500):
            request_id = f"job-{index}"
            try:
                publisher.publish(request_id)
                accepted.add(request_id)
            except video_cancel.CancelDeliveryError:
                rejected.add(request_id)
        assert rejected, "The burst must exercise backpressure"
        for _ in range(500):
            listener.is_cancelled([])
            if accepted.issubset(listener._cancelled):
                break
            time.sleep(0.01)
        assert accepted.issubset(listener._cancelled)
        for request_id in rejected:
            _deliver_cancel(publisher.publish, listener, request_id)
        assert all(listener._is_cancelled(f"job-{i}") for i in range(3500))
    finally:
        context.destroy(linger=0)
        publisher.close()


def test_cancel_waits_for_all_replica_subscriptions():
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint, replicas=2)
    context = zmq.Context()
    first = CancelListener(context, endpoint)
    try:
        for _ in range(100):
            with pytest.raises(video_cancel.CancelDeliveryError):
                publisher.publish("job")
            time.sleep(0.001)
        second = CancelListener(context, endpoint)
        _deliver_cancel(publisher.publish, second)
        assert first.is_cancelled(["job"])
        first._socket.close(linger=0)
        for _ in range(500):
            try:
                publisher.publish("next")
            except video_cancel.CancelDeliveryError:
                break
            time.sleep(0.01)
        else:
            pytest.fail("Disconnected replica was not detected")
    finally:
        context.destroy(linger=0)
        publisher.close()


def test_idle_scheduler_drains_cancellations(monkeypatch):
    endpoint = _cancel_endpoint()
    publisher = video_cancel.CancelPublisher(endpoint)
    context = zmq.Context()
    listener = CancelListener(context, endpoint)
    monkeypatch.setattr(request_cancel, "_listener", listener)
    scheduler = object.__new__(Scheduler)
    scheduler.receiver = Mock()
    scheduler.receiver.recv_multipart.side_effect = zmq.Again
    scheduler._socket_lock = threading.Lock()
    scheduler.server_args = SimpleNamespace(
        comfyui_mode=False, sp_degree=1, enable_cfg_parallel=False, tp_size=1
    )
    try:
        _deliver_cancel(publisher.publish, listener, "ready")
        publisher.publish("idle")
        for _ in range(500):
            assert scheduler.recv_reqs() == []
            if "idle" in listener._cancelled:
                break
            time.sleep(0.01)
        assert "idle" in listener._cancelled
    finally:
        context.destroy(linger=0)
        publisher.close()

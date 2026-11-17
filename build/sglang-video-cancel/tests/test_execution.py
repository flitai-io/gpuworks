# SPDX-License-Identifier: Apache-2.0
# Backported from SGLang PR 47055, commit 8b0f2f929f05a4eb447fe143ecd3ba54e688f33a.
import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock
import pytest
from tqdm.auto import tqdm
import sglang.multimodal_gen.runtime.managers.gpu_worker as gpu_worker_module
from sglang.multimodal_gen.configs.pipeline_configs.wan import WanT2V480PConfig
from sglang.multimodal_gen.configs.sample.sampling_params import SamplingParams
from sglang.multimodal_gen.runtime.entrypoints.openai import utils as openai_utils
from sglang.multimodal_gen.runtime.managers import request_cancel
from sglang.multimodal_gen.runtime.managers import scheduler as scheduler_module
from sglang.multimodal_gen.runtime.managers.gpu_worker import GPUWorker
from sglang.multimodal_gen.runtime.managers.cancel_tqdm import CancelAwareTqdm
from sglang.multimodal_gen.runtime.managers.request_cancel import RequestCancelledError, raise_if_cancelled
from sglang.multimodal_gen.runtime.managers.scheduler import Scheduler
from sglang.multimodal_gen.runtime.pipelines_core.executors import pipeline_executor as pipeline_executor_module
from sglang.multimodal_gen.runtime.pipelines_core.executors.sync_executor import SyncExecutor
from sglang.multimodal_gen.runtime.pipelines_core.schedule_batch import OutputBatch, Req
from sglang.multimodal_gen.runtime.pipelines_core.stages import base as stage_base
from sglang.multimodal_gen.runtime.pipelines_core.stages.base import StageParallelismType
from sglang.multimodal_gen.runtime.platforms import current_platform
from sglang.multimodal_gen.runtime.utils.perf_logger import StageProfiler
from video_cancel_helpers import _req, cancel_state


def test_step_check_runs_after_each_step():
    calls = []
    with CancelAwareTqdm(
        total=3,
        disable=True,
        on_step=lambda: calls.append("update"),
    ) as bar:
        for _ in range(3):
            bar.update()
    # mininterval=0 makes tqdm call update() from its own __iter__ every step
    for _ in CancelAwareTqdm(
        range(3),
        mininterval=0,
        on_step=lambda: calls.append("iter"),
    ):
        pass
    assert calls == ["update"] * 3 + ["iter"] * 3


def test_step_check_stops_the_loop(cancel_state):
    batch = _req()
    steps = 0
    with pytest.raises(RequestCancelledError):
        with CancelAwareTqdm(
            total=10,
            disable=True,
            on_step=request_cancel.step_cancel_check(batch),
        ) as bar:
            for step in range(10):
                steps += 1
                if step == 2:
                    cancel_state("job")
                bar.update()
    assert steps == 3


class _Stage(stage_base.PipelineStage):
    def __init__(self, parallelism=StageParallelismType.REPLICATED):
        self._progress_range = (0.0, 1.0)
        self._parallelism = parallelism

    @property
    def parallelism_type(self):
        return self._parallelism

    def forward(self, batch, server_args):
        return batch


@pytest.mark.parametrize(
    "batch, parallelism, checked",
    [
        (_req(), StageParallelismType.REPLICATED, True),
        (_req(), StageParallelismType.CFG_PARALLEL, True),
        (_req(), StageParallelismType.MAIN_RANK_ONLY, False),
        (_req(), StageParallelismType.MAIN_RANK_ONLY_AND_SEND_TO_OTHERS, False),
        (_req(cancellable=False), StageParallelismType.REPLICATED, False),
        (_req(is_warmup=True), StageParallelismType.REPLICATED, False),
    ],
)
def test_progress_bar_checks_only_replicated_stages(
    cancel_state, monkeypatch, batch, parallelism, checked
):
    bar = _Stage(parallelism).progress_bar(total=3, batch=batch, disable=True)
    if checked:
        assert type(bar) is CancelAwareTqdm and bar._on_step is not None
    else:
        assert type(bar) is tqdm


def _bare_worker(monkeypatch):
    worker = GPUWorker.__new__(GPUWorker)
    worker.is_output_rank = True
    worker._realtime_sessions = SimpleNamespace(attach=Mock())
    worker._release_warmup_pool = Mock()
    worker._record_output_peak_memory = Mock()
    monkeypatch.setattr(current_platform, "is_cpu", lambda: True)
    return worker


def _cancelled(_reqs):
    raise RequestCancelledError(["job"])


def test_worker_skips_forward_of_cancelled_request(monkeypatch):
    worker = _bare_worker(monkeypatch)
    monkeypatch.setattr(gpu_worker_module, "raise_if_cancelled", _cancelled)
    forward = Mock()

    result = worker._execute_forward_common(
        SimpleNamespace(is_warmup=False, extra={}),
        forward_fn=forward,
        log_reqs=[],
        return_req=False,
        save_output_paths=Mock(),
        error_context="request job",
    )

    forward.assert_not_called()
    assert isinstance(result, OutputBatch)
    assert result.cancelled and "cancelled" in result.error


def test_stage_profiler_does_not_log_a_cancel_as_an_error():
    logger = Mock()
    profiler = StageProfiler("DenoisingStage", logger=logger, metrics=None)
    profiler.log_stage_start_end = True
    profiler.prometheus = None
    with pytest.raises(RequestCancelledError):
        with profiler:
            raise RequestCancelledError(["job"])
    logger.error.assert_not_called()
    with pytest.raises(RuntimeError):
        with profiler:
            raise RuntimeError("stage failed")
    logger.error.assert_called_once()


def test_cancelled_output_raises_a_cancel(monkeypatch):
    client = SimpleNamespace(
        forward=AsyncMock(
            return_value=OutputBatch(error="Request job was cancelled", cancelled=True)
        )
    )
    error = Mock()
    monkeypatch.setattr(openai_utils.logger, "error", error)
    batch = Req(sampling_params=SamplingParams(prompt="p", seed=1, request_id="job"))

    with pytest.raises(RequestCancelledError, match="job"):
        asyncio.run(openai_utils.process_generation_batch(client, batch))
    error.assert_not_called()


def test_sequential_worker_does_not_skip_a_request(monkeypatch):
    """A skipped request would leave its output to the next request of the group."""
    worker = _bare_worker(monkeypatch)
    monkeypatch.setattr(gpu_worker_module, "raise_if_cancelled", _cancelled)
    output = OutputBatch()

    result = worker._execute_forward_common(
        SimpleNamespace(is_warmup=False, extra={}),
        forward_fn=lambda: output,
        log_reqs=[],
        return_req=False,
        save_output_paths=Mock(),
        error_context="grouped request job",
        propagate_forward_errors=True,
        check_cancelled=False,
    )

    assert result is output


@pytest.mark.parametrize("propagate", [False, True])
def test_worker_reports_cancel_inside_forward(monkeypatch, propagate):
    worker = _bare_worker(monkeypatch)

    def run():
        return worker._execute_forward_common(
            SimpleNamespace(is_warmup=False, extra={}),
            forward_fn=lambda: _cancelled([]),
            log_reqs=[],
            return_req=False,
            save_output_paths=Mock(),
            error_context="request job",
            propagate_forward_errors=propagate,
        )

    if propagate:
        with pytest.raises(RequestCancelledError):
            run()
    else:
        assert "cancelled" in run().error


def _sequential_worker(monkeypatch):
    """Return a worker whose pipeline yields one output per request it runs."""
    worker = _bare_worker(monkeypatch)
    worker.server_args = SimpleNamespace(
        pipeline_config=SimpleNamespace(
            supports_sequential_multi_output_inference=lambda: False
        )
    )
    worker._execute_forward_common = lambda req, *, forward_fn, **kwargs: forward_fn()
    groups = []
    outputs = {}

    def forward_batch_sequentially(reqs, server_args):
        groups.append([req.request_id for req in reqs])
        return (outputs.setdefault(req.request_id, OutputBatch()) for req in reqs)

    worker.pipeline = SimpleNamespace(
        forward_batch_sequentially=forward_batch_sequentially
    )
    return worker, groups, outputs


def test_sequential_worker_skips_a_cancelled_request(monkeypatch, cancel_state):
    worker, groups, outputs = _sequential_worker(monkeypatch)
    cancel_state("a")

    results = list(worker.execute_forward_sequentially([_req("a"), _req("b")]))

    assert groups == [["b"]]
    assert results[0].cancelled and "cancelled" in results[0].error
    assert results[1] is outputs["b"]


def test_sequential_worker_does_not_start_a_cancelled_group(monkeypatch, cancel_state):
    worker, groups, _ = _sequential_worker(monkeypatch)
    cancel_state("a")

    results = list(worker.execute_forward_sequentially([_req("a")]))

    assert groups == []
    assert len(results) == 1 and results[0].cancelled


def _sequential_executor(execute):
    executor = object.__new__(SyncExecutor)
    executor.execute_group = lambda stages, batches, server_args: batches
    executor.execute = execute
    first_stage = SimpleNamespace(
        iter_sequential_requests=lambda batch, server_args: iter((batch,))
    )
    return executor, [first_stage, object()]


def test_sequential_outputs_stop_after_a_cancel(monkeypatch, cancel_state):
    error = Mock()
    monkeypatch.setattr(pipeline_executor_module.logger, "error", error)
    executed = []

    def execute(stages, batch, server_args):
        executed.append(batch.request_id)
        # the client cancels the job while its first output denoises
        cancel_state("job:0", "job:1", "job:2")
        raise RequestCancelledError(["job"])

    executor, stages = _sequential_executor(execute)
    batches = [_req(f"job:{i}") for i in range(3)]

    outputs = list(executor.execute_group_sequentially(stages, batches, None))

    assert executed == ["job:0"]
    assert [output.cancelled for output in outputs] == [True, True, True]
    error.assert_not_called()


def test_sequential_cancel_skips_only_the_cancelled_request(cancel_state):
    executed = []

    def execute(stages, batch, server_args):
        executed.append(batch.request_id)
        return OutputBatch()

    executor, stages = _sequential_executor(execute)
    cancel_state("job")

    outputs = list(
        executor.execute_group_sequentially(stages, [_req("job"), _req("other")], None)
    )

    assert executed == ["other"]
    assert [output.cancelled for output in outputs] == [True, False]


@pytest.mark.parametrize(
    "first_error, cancelled", [(None, True), ("Error executing job:0", False)]
)
def test_merged_outputs_keep_the_cancel(first_error, cancelled):
    merged = GPUWorker._merge_expanded_output_batches(
        [
            OutputBatch(error=first_error),
            OutputBatch(error="Request job:1 was cancelled", cancelled=True),
        ]
    )
    assert merged.error is not None
    assert merged.cancelled is cancelled




@pytest.mark.parametrize(
    "members, cancellable",
    [
        ([(True, True), (True, True)], True),
        ([(True, True), (False, True)], False),
        # Cancellation is independent of progress reporting.
        ([(True, True), (True, False)], True),
    ],
)
def test_merged_batch_is_cancellable_only_when_each_member_is(
    monkeypatch, members, cancellable
):
    scheduler = object.__new__(Scheduler)
    monkeypatch.setattr(scheduler, "_can_dynamic_batch", lambda base, other: True)
    reqs = []
    for index, (member_cancellable, report_progress) in enumerate(members):
        req = _req(f"job{index}", cancellable=member_cancellable)
        req.report_progress = report_progress
        reqs.append(req)

    merged = scheduler._try_merge_generation_reqs(reqs)

    assert merged.cancellable is cancellable
    assert merged.client_request_ids() == [req.request_id for req in reqs]


@pytest.mark.parametrize("cancelled", [(), ("b",), ("a", "b"), ("a", "b", "c")])
def test_queued_cancel_is_filtered_before_dynamic_batch(cancel_state, cancelled):
    scheduler = object.__new__(Scheduler)
    scheduler._async_output_save = False
    scheduler.server_args = SimpleNamespace(pipeline_config=WanT2V480PConfig())
    scheduler._batching_max_size = 3
    scheduler._batching_delay_s = 0
    reqs = [_req(name) for name in ("a", "b", "c")]
    for req in reqs:
        req.report_progress = True
    assert scheduler.server_args.pipeline_config.supports_dynamic_batching()
    assert scheduler._can_dynamic_batch(reqs[0], reqs[1])
    executed = []

    def forward(batches):
        raise_if_cancelled(batches)
        ids = [rid for batch in batches for rid in batch.client_request_ids()]
        executed.append(ids)
        return OutputBatch(output=ids)

    scheduler.worker = SimpleNamespace(
        is_sleeping=lambda: False, execute_forward=forward
    )
    scheduler._split_batched_output = lambda output, batches: [
        OutputBatch(output=[batch.request_id]) for batch in batches
    ]
    cancel_state(*cancelled)

    outputs = scheduler._handle_generation(reqs)

    live = [req.request_id for req in reqs if req.request_id not in cancelled]
    assert executed == ([live] if live else [])
    for req, output in zip(reqs, outputs, strict=True):
        assert output.cancelled is (req.request_id in cancelled)
        if not output.cancelled:
            assert output.output == [req.request_id]


def test_explicit_group_keeps_single_reply(cancel_state):
    scheduler = object.__new__(Scheduler)
    scheduler._async_output_save = False
    scheduler.server_args = SimpleNamespace(pipeline_config=WanT2V480PConfig())
    output = OutputBatch(output=["grouped output"])
    forward = Mock(return_value=output)
    scheduler.worker = SimpleNamespace(
        is_sleeping=lambda: False, execute_forward=forward
    )
    reqs = [_req("cancelled"), _req("live"), _req("another")]
    cancel_state("cancelled")

    assert scheduler._handle_generation(reqs, allow_dynamic_batching=False) is output
    forward.assert_called_once_with(reqs)


@pytest.mark.parametrize("streaming", [False, True])
def test_filtered_batch_preserves_reply_mode(cancel_state, streaming):
    scheduler = object.__new__(Scheduler)
    scheduler._async_output_save = not streaming
    scheduler.server_args = SimpleNamespace(
        pipeline_config=SimpleNamespace(
            supports_sequential_multi_output_inference=lambda: streaming,
        )
    )
    output = OutputBatch(output=["live"])
    result = (
        scheduler_module._SequentiallyReturnedOutputs(iter([output]))
        if streaming
        else output
    )
    scheduler.worker = SimpleNamespace(
        is_sleeping=lambda: False, execute_forward=lambda reqs: result
    )
    scheduler._iter_grouped_outputs_sequentially = lambda reqs: result.outputs
    live = _req("live")
    live.num_outputs_per_prompt = 2 if streaming else 1
    cancel_state("cancelled")

    outputs = scheduler._handle_generation([_req("cancelled"), live])

    if streaming:
        assert isinstance(outputs, scheduler_module._SequentiallyReturnedOutputs)
        outputs = list(outputs.outputs)
    assert outputs[0].cancelled
    assert outputs[1] is (output if streaming else result)


@pytest.mark.parametrize("native_grouped", [False, True])
def test_cancelled_batch_returns_cancelled_outputs(monkeypatch, native_grouped):
    """A merged dynamic batch and a native grouped batch keep the cancel."""
    scheduler = object.__new__(Scheduler)
    scheduler._async_output_save = False
    scheduler.server_args = SimpleNamespace(
        pipeline_config=SimpleNamespace(
            supports_sequential_multi_output_inference=lambda: False,
            supports_native_grouped_requests=lambda: native_grouped,
            supports_sequential_dit_inference=lambda: False,
        )
    )
    scheduler.worker = SimpleNamespace(
        is_sleeping=lambda: False,
        execute_forward=Mock(
            return_value=OutputBatch(
                error="Request job0, job1 was cancelled", cancelled=True
            )
        ),
    )
    monkeypatch.setattr(scheduler, "_try_merge_generation_reqs", lambda reqs: reqs[0])
    error = Mock()
    monkeypatch.setattr(scheduler_module.logger, "error", error)

    outputs = scheduler._handle_generation([_req("job0"), _req("job1")])

    assert [output.cancelled for output in outputs] == [True, True]
    assert all("cancelled" in output.error for output in outputs)
    error.assert_not_called()

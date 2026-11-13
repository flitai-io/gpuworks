# MinerU logits processor

Source: [mineru-vl-utils](https://github.com/opendatalab/mineru-vl-utils/tree/c9797cde8f54cc48306196fc6aebe73ea31dd4a3).

`mineru_logits_processor.py` is an unmodified copy of
`mineru_vl_utils/logits_processor/vllm_v1_no_repeat_ngram.py`, under Apache-2.0
(see LICENSE.txt). It implements the legacy vLLM V1 batch interface. The
catalog asset provides this module without installing the PDF
client or replacing the image's Torch stack during deployment.

MinerU sends `vllm_xargs.no_repeat_ngram_size`. The processor maintains request
state across removals and batch moves. It is needed for the specialized
document recognition prompts, not general conversation.

Run `python3 build/inference-templates/mineru/package.py` from the repository
root after updating these files, then update the asset digest and size in
`model.yaml`. The archive has deterministic timestamps and permissions.

The recipe explicitly sets `VLLM_USE_V2_MODEL_RUNNER=0`. vLLM 0.31.0 retains
the legacy V1 interface but defaults to a V2 loader that rejects this class.
Keeping the old import path available is not enough to establish compatibility.
See the [vLLM 0.31 interface documentation](https://docs.vllm.ai/en/v0.31.0/features/custom_logitsprocs/).
The upstream module, including its ImportError guidance, is preserved verbatim.
Do not remove the runner selection when updating the image without porting
and testing the processor against the new interface.

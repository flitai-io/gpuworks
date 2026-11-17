# Laya Typed Decisions

Laya Typed Decisions is the fine-tuned checkpoint of Convai Innovations' **System One decision model** (an open-weight Jev): a ModernBERT-large encoder (421M parameters) fine-tuned on typed-decisions workflows (invoice processing, security incidents, customer service, agent observability). It does not generate text: given a shared `state` (text or JSON) and a set of typed questions, one forward pass returns probabilities for every question.

## Question types

| Type | criteria | Answer |
|---|---|---|
| `noul` | Optional; may describe the two values with `false` / `true` | `noul`, the probability of yes |
| `choice` | An object of option name → description, or a list of option names (up to 100) | `choice`, the selected option, `probabilities` and `confidence` |
| `score` | An ordered list of level descriptions (up to 32; values 0, 1, 2, …) | `score`, the probability-weighted expected level, `probabilities` and `legend` |

Each request takes 1–64 questions with at most 512 options across them, and a state of at most 50,000 characters. Reads English only. A state longer than 1024 tokens is **truncated**, not refused.
Text only: a request with `images` is refused.

## Calling it

Once deployed, call it through the gateway's `POST /v1/systemone` (TypeSafe's System One wire format); the Playground's Decision page builds questions and shows the probabilities:

```bash
curl "$GPUWORKS_URL/v1/systemone" -H "Authorization: Bearer $GPUWORKS_API_KEY" -H "Content-Type: application/json" -d '{
  "model": "<service name>",
  "state": "Customer: I was charged twice for the same order, please refund the extra payment.",
  "questions": {
    "team": {"type": "choice", "instructions": "Which team should handle this?", "criteria": {"billing": "Payments and refunds", "delivery": "Shipping"}},
    "urgency": {"type": "score", "instructions": "How urgent is it?", "criteria": ["Low", "Medium", "High"]},
    "refund_requested": {"type": "noul", "instructions": "Does the customer ask for a refund?"}
  }
}'
```

The response carries only the System One contract's fields (`model`, `answers`, `usage`). `usage.input_tokens` counts input tokens; `usage.output_tokens` is always 0.

## What it is good for

English invoice checks, security incident grading, support triage and agent action checks; the model card reports 0.766 on typed-decisions, against about 0.36 zero-shot for the base checkpoints. Limits stated on the model card: accuracy drops noticeably past 20 options; `noul` can follow its `false:` / `true:` descriptions rather than the state,
so use a two-option `choice` when a yes/no answer must be stable; probabilities ship over-confident, so recalibrate on your own data before gating on a threshold.

## Notes

- The inference code is the image's `laya` package (0.4.31); the weights directory stays read-only and the tokenizer config is patched on a copy inside the container.
- The weights are about 0.85 GB.
- License: Apache-2.0.
- Model page: <https://huggingface.co/convaiinnovations/laya-typed-decisions>

# Intern-Decision-2B

Intern-Decision is a **System One decision model** (an open-weight Jev) from InternLM, fine-tuned from Qwen3.5-2B. It does not generate text:
given a shared `state` (text or JSON, with up to 8 images) and a set of typed questions, one forward pass returns calibrated probabilities for every question.

## Question types

| Type | criteria | Answer |
|---|---|---|
| `noul` | Optional; may describe the two values with `yes` / `no` | `noul`, the probability of yes |
| `choice` | An ordered object of option name → description (up to 62) | `choice`, the selected option, and `probabilities` |
| `score` | An ordered list of level descriptions (values 0, 1, 2, …) | `score`, the probability-weighted expected level, and `legend` |

A request carries 1–16 questions and at most 8192 input tokens (image tokens included); longer input is rejected, never truncated. Every answer also has `confidence` (the largest candidate probability) and `decision` (the most likely value).

## Calling it

Once deployed, call `POST /v1/systemone` on the gateway (TypeSafe's System One wire format), or build questions on the Playground's Decision page:

```bash
curl "$GPUWORKS_URL/v1/systemone" -H "Authorization: Bearer $GPUWORKS_API_KEY" -H "Content-Type: application/json" -d '{
  "model": "<service name>",
  "state": "The customer was charged twice and asks for the extra payment back.",
  "questions": {
    "team": {"type": "choice", "instructions": "Which team should handle this request?", "criteria": {"billing": "Payments and refunds", "delivery": "Shipping and delivery"}},
    "urgency": {"type": "score", "instructions": "Rate the priority.", "criteria": ["Low", "Medium", "High"]},
    "refund_requested": {"type": "noul", "instructions": "Is the customer asking for a refund?"}
  }
}'
```

Images go in an `images` array as `data:image/png;base64,...` (jpeg and webp also work), at most 4 MiB each.
The reply's `usage.input_tokens` counts input tokens; `usage.output_tokens` counts answered questions, not generated tokens.

## Good for

Routing and triage, bulk labeling, approving an agent's tool call or answer, branch decisions in a workflow. It does no multi-step reasoning and gives no explanation;
it can get numbers and double negatives wrong, so give `choice` questions an "other" option.

## Notes

- Probabilities are temperature-calibrated (the 2B temperature is in the weights' `inference.py`); calibration changes the probabilities, never the most likely option.
- The inference code ships with the weights (`inference.py`); the platform runs only the copy whose SHA-256 the template pins.
- License: Apache-2.0, with Qwen's license kept (`LICENSE-QWEN`).
- Model page: <https://huggingface.co/internlm/Intern-Decision-2B>

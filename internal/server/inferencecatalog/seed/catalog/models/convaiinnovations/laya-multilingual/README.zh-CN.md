# Laya Multilingual

Laya Multilingual 是 Convai Innovations 发布的 **System One 决策模型**（“开源版 Jev”）的多语言检查点，编码器是 mmBERT-base（3.22 亿参数），读 100 多种语言，包括中文。它不生成文本：给一段共享的 `state`（文本或 JSON）和一组带类型的问题，一次前向就给出每个问题的概率。

## 三种问题

| 类型 | criteria | 回答 |
|---|---|---|
| `noul` | 可省略；可用 `false` / `true` 描述两种取值 | `noul` 为“是”的概率 |
| `choice` | 选项名 → 描述的对象，或选项名列表（最多 100 个） | `choice` 选中项，`probabilities` 各项概率，`confidence` |
| `score` | 有序等级描述列表（最多 32 级，值依次为 0、1、2…） | `score` 概率加权的期望等级，`probabilities`，`legend` 等级说明 |

每次请求 1–64 个问题，所有问题的选项合计不超过 512 个，状态不超过 50000 个字符。状态最长 1024 token，超出部分**被截断**而不是拒绝。
只收文本，带 `images` 的请求会被拒绝。

## 调用

部署后通过网关的 `POST /v1/systemone` 调用（TypeSafe System One 线格式），试验场的「决策」页可以直接构造问题看概率：

```bash
curl "$GPUWORKS_URL/v1/systemone" -H "Authorization: Bearer $GPUWORKS_API_KEY" -H "Content-Type: application/json" -d '{
  "model": "<服务名>",
  "state": "Customer: I was charged twice for the same order, please refund the extra payment.",
  "questions": {
    "team": {"type": "choice", "instructions": "Which team should handle this?", "criteria": {"billing": "Payments and refunds", "delivery": "Shipping"}},
    "urgency": {"type": "score", "instructions": "How urgent is it?", "criteria": ["Low", "Medium", "High"]},
    "refund_requested": {"type": "noul", "instructions": "Does the customer ask for a refund?"}
  }
}'
```

回复只带 System One 契约里的字段（`model`、`answers`、`usage`）。`usage.input_tokens` 是输入 token 数，`usage.output_tokens` 恒为 0。

## 适合做什么

中文或多语言的客服工单路由、意图识别、内容分类与紧急程度打分。模型卡写明的限制：选项超过 20 个时准确率明显下降；`noul` 有时跟着 `false:` / `true:` 的描述文字走而不看状态，
需要稳定的是非判断时可以改用两个选项的 `choice`；出厂概率偏自信，要按阈值放行时建议先用自己的数据重新校准。

## 说明

- 推理代码是镜像里的 `laya` 包（0.4.31）；权重目录只读，tokenizer 配置在容器内的副本上修补。
- 权重约 0.7 GB。
- 许可：Apache-2.0。
- 模型主页：<https://huggingface.co/convaiinnovations/laya-multilingual>

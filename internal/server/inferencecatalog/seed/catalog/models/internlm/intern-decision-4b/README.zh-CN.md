# Intern-Decision-4B

Intern-Decision 是 InternLM 发布的 **System One 决策模型**（“开源版 Jev”），基于 Qwen3.5-4B 微调。它不生成文本：
给一段共享的 `state`（文本或 JSON，可附最多 8 张图）和一组带类型的问题，一次前向就给出每个问题的校准概率。

## 三种问题

| 类型 | criteria | 回答 |
|---|---|---|
| `noul` | 可省略；可用 `yes` / `no` 描述两种取值 | `noul` 为“是”的概率 |
| `choice` | 选项名 → 描述的有序对象（最多 62 个） | `choice` 选中项，`probabilities` 各项概率 |
| `score` | 有序等级描述列表（值依次为 0、1、2…） | `score` 概率加权的期望等级，`legend` 等级说明 |

每次请求 1–16 个问题，输入（含图片 token）不超过 8192 token，超出直接拒绝，不截断。每个回答还带 `confidence`（最大候选概率）与 `decision`（概率最大的取值）。

## 调用

部署后通过网关的 `POST /v1/systemone` 调用（TypeSafe System One 线格式），试验场的「决策」页可以直接构造问题看概率：

```bash
curl "$GPUWORKS_URL/v1/systemone" -H "Authorization: Bearer $GPUWORKS_API_KEY" -H "Content-Type: application/json" -d '{
  "model": "<服务名>",
  "state": "客户说被重复扣了两次款，要求退回多扣的钱。",
  "questions": {
    "team": {"type": "choice", "instructions": "该由哪个团队处理？", "criteria": {"billing": "支付与退款", "delivery": "物流配送"}},
    "urgency": {"type": "score", "instructions": "紧急程度", "criteria": ["低", "中", "高"]},
    "refund_requested": {"type": "noul", "instructions": "客户是否要求退款？"}
  }
}'
```

图片放在 `images` 数组里，写成 `data:image/png;base64,...`（也接受 jpeg、webp），单张不超过 16 MiB。
回复的 `usage.input_tokens` 是输入 token 数；`usage.output_tokens` 是回答的问题数，不是生成的 token。

## 适合做什么

请求路由与分诊、批量打标签、对 agent 的工具调用或回答做放行检查、工作流里的分支判断。它不做多步推理，也不解释原因；
数字比较和双重否定容易出错，`choice` 问题建议留一个“其他”选项。

## 说明

- 概率经过温度校准（4B 的温度见权重仓库的 `inference.py`），校准只改概率、不改最可能的选项。
- 推理代码随权重仓库（`inference.py`），平台只运行模板钉住 SHA-256 的那一份。
- 许可：Apache-2.0，并保留 Qwen 的许可（`LICENSE-QWEN`）。
- 模型主页：<https://huggingface.co/internlm/Intern-Decision-4B>

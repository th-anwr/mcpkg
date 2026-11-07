# Writing 50k descriptions with an open model, cheaply and without slop

## What the model is allowed to do

The model writes **text only**: a one-line summary, a 2–4 sentence description, 1–3 categories from a fixed list, and "what is this key and where do I get it" hints. Everything else on a page comes from verified data:

- install commands
- tool lists
- scores and versions
- environment variables

A model mistake can only make a sentence worse. It can never break an install or change a grade.

## Guardrails (all in `internal/stages/enrich.go`)

1. **Small, clean input.** The README excerpt has code blocks, images and HTML stripped (~6,000 chars). It's sent with registry metadata, env var names and the live tool list. The README is labelled untrusted, so instructions inside it are not followed.
2. **Structured output.** JSON mode, then strict parsing.
3. **Grounding checks.** Any failure triggers one retry with the errors, then a deterministic fallback:
   - every URL must appear in the input
   - every `snake_case` / backticked tool name must exist in `tools/list`
   - no install commands, markdown, or "as an AI…" text
   - categories must come from the taxonomy
   - length limits
4. **Fallback.** First sentence of the registry/package description, the first README paragraphs, keyword categories. Pages are never empty.
5. **Human sampling.** 1% of accepted outputs (`LLM_REVIEW_SAMPLE`), plus every tool with ≥1,000 stars, go to `/admin/review`.
6. **Measure before spending.** `mcpkg eval-llm -n 100 -price-in X -price-out Y` prints acceptance rate, top rejection reasons, average tokens, and the projected cost for 50k tools.

## Size of the job

| | Per tool | 50k tools |
|---|---|---|
| Input | ~1.5–2.5k tokens | ~100M tokens |
| Output | ~150–250 tokens | ~10M tokens |
| Retries | +10–20% on rejected outputs | |

## Options (prices change; check before you run)

| Option | Example models | Rough cost for 50k | Effort | Notes |
|---|---|---|---|---|
| **Hosted open-model API** (recommended start) | Llama 3.1 8B, gpt-oss-20b, Qwen3 on DeepInfra, Together, OpenRouter… | **~$3–10** (e.g. Llama 3.1 8B ≈ $0.02/M in + $0.04/M out) | none | Paid outside AWS credits. Use the batch or "flex" tier if offered. |
| **Amazon Bedrock open-weight models** | gpt-oss, Qwen3, Llama | ~$5–15 (less with batch inference) | low | May be covered by your AWS credits: check Billing → Credits → eligible services first. Bedrock has an OpenAI-compatible endpoint. |
| **Self-host vLLM on GPU spot** | Qwen2.5/Qwen3 7–8B AWQ on g4dn.xlarge (T4) | ~$5–12 (g4dn.xlarge spot ≈ $0.32/h × 15–30 h) | medium | Uses AWS credits. New accounts need a G-instance spot quota increase. Script: `deploy/aws/llm-gpu-spot.sh`. |
| CPU-only (llama.cpp / Ollama on the worker) | 3–4B models | "free" | low | Too slow for 50k (weeks). Fine for the ~100 new tools/day after launch. |

Any OpenAI-compatible endpoint works. Set three env vars:

```
LLM_BASE_URL=https://api.deepinfra.com/v1/openai   # or http://<gpu-box>:8000/v1, OpenRouter, Bedrock…
LLM_API_KEY=...
LLM_MODEL=openai/gpt-oss-20b
```

## Procedure

1. Run health on everything first (READMEs get fetched there).
2. `mcpkg eval-llm -n 100 -price-in 0.03 -price-out 0.14` on 2–3 candidate models.
3. Pick the cheapest model with ≥90% first-try acceptance. Skim the 5 printed samples and `/admin/review`.
4. Let the pipeline run. The enrich stage has its own concurrency in `pipeline.json`. Keep it within the provider's rate limits.
5. After the bulk run, switch to the cheapest option that handles daily new tools (often the same API, or a CPU model).

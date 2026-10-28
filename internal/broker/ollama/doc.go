// Package ollama is the model-endpoint broker (spec: "Ollama / model
// endpoints"): a reverse proxy in front of the host's Ollama (or any
// OpenAI-compatible endpoint such as vLLM or LM Studio) that sandboxes reach
// as http://ollama.mycel.internal and trusted-host agents through the
// endpoint returned by Attach.
//
// Contract (owned by the orchestrator):
//   - Each attached grant gets its own bearer credential (the sandbox sees
//     it in OLLAMA_HOST/OPENAI_BASE_URL + an Authorization header the daemon
//     injects into the sandbox environment); requests without a valid
//     credential are refused, so sandboxes cannot use each other's grants.
//   - Allowed paths: /api/generate, /api/chat, /api/embeddings, /api/embed,
//     /api/tags, /api/show, /v1/chat/completions, /v1/completions,
//     /v1/embeddings, /v1/models. /api/pull and /api/delete only with
//     allow_pull/allow_delete. Everything else -> 403. The model in every
//     request body must be in the grant's allowlist (empty = any model
//     already present).
//   - Rate limit requests_per_minute per grant; max_tokens counts generated
//     tokens from the responses (streaming and non-streaming) and refuses
//     further requests once exceeded.
//   - Usage per grant (requests, generated tokens, model) is reported through
//     Usage(); every request is audited with the model and token counts.
//   - The upstream address comes from the node (default http://127.0.0.1:11434).
//
// Implementation notes (v1):
//   - The credential is accepted as Bearer, as Basic grant:<token>, or as the
//     token query parameter; it is never forwarded, logged or audited.
//   - max_tokens is enforced per request: the remaining budget is reserved
//     when a request is admitted, generation is clamped to it
//     (options.num_predict, or max_tokens for OpenAI-compatible requests) and
//     a stream that still exceeds it is cut. Until a final count arrives every
//     streamed chunk counts as one token, so cut or aborted streams are
//     charged; oversized non-streaming bodies are charged at 4 bytes per
//     token.
//   - Bodies whose model, name, stream, stream_options, options, max_tokens,
//     max_completion_tokens or options.num_predict keys are duplicated or not
//     in canonical case are refused, so every upstream reads what was checked.
//   - At most 8 requests per grant are in flight and 256 connections open; a
//     client has 30 s to send its body. Refusals without a valid credential
//     are audited at a sampled rate.
package ollama

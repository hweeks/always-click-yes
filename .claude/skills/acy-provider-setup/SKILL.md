---
name: acy-provider-setup
description: Configure always-click-yes (acy) to run Claude Code through a local LiteLLM gateway using OpenAI, Cerebras, Fireworks, or OpenRouter credentials, or connect it to a local vLLM server. Use when a user asks to use a non-Anthropic model/provider with acy, configure provider keys, select provider models, or diagnose gateway setup.
---

# ACY provider setup

Keep Claude Code as the runtime. For hosted OpenAI-compatible providers, let
acy start a private LiteLLM sidecar; it translates the Messages API and keeps
the upstream key out of Claude's child processes.

## Configure a hosted provider

1. Ask for the provider and model; default only when the user has no preference.
2. Verify the matching key is present without printing it:

   - OpenAI: `OPENAI_API_KEY`
   - Cerebras: `CEREBRAS_API_KEY`
   - Fireworks: `FIREWORKS_API_KEY`
   - OpenRouter: `OPENROUTER_API_KEY`

3. Ensure LiteLLM is available: `pipx install 'litellm[proxy]'`.
4. Put only non-secret settings in `.acy.json`, for example:

   ```json
   { "provider": "openai", "model": "gpt-4.1", "childModel": "gpt-4.1" }
   ```

5. Start with `acy run`. acy starts LiteLLM on loopback, creates a random local
   gateway token, and removes the upstream key from the Claude process.

Use `--gateway-bin /path/to/litellm` when LiteLLM is not on PATH. Do not put
provider keys in `.acy.json`, command arguments, logs, or generated files.

## Configure local vLLM

Start vLLM with an Anthropic Messages-compatible endpoint, then use:

```json
{ "provider": "vllm", "gatewayUrl": "http://127.0.0.1:8000", "model": "your-model", "childModel": "your-model" }
```

`gatewayUrl` defaults to `http://127.0.0.1:8000`. Confirm tool calling works
with a small supervised task before allowing unattended code changes.

## Diagnose failures

- Missing-key errors: export the provider-specific environment variable in the
  process that launches acy.
- Cannot find LiteLLM: install it with pipx or set `gatewayBin`.
- A model fails before work begins: verify the exact provider model identifier
  and set both `model` and `childModel` to names the provider exposes.
- Tool calls fail: treat that model/provider pairing as experimental; switch to
  a known tool-capable model or Anthropic while collecting the acy debug log.

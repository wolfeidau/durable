# Providers and protocols

Choose where the agent sends requests, which wire format it uses, and what each route can't do.

## Providers and protocols

The provider is where requests go and which key they use. The protocol is the wire format, and it
is also the format of the stored model replies.

| Provider | Key | Default protocol | Allowed protocols |
|---|---|---|---|
| `anthropic` | SDK defaults | `anthropic-messages` | `anthropic-messages` |
| `openrouter` | `OPENROUTER_API_KEY` | `openai-responses` | `openai-responses`, `anthropic-messages` |

```sh
./durable                                                    # anthropic, anthropic-messages
./durable --provider openrouter --model openai/gpt-5.6-luna  # openai-responses
./durable --provider openrouter --protocol anthropic-messages --model anthropic/claude-sonnet-5-5
```

Every start prints a `[model] <provider> via <protocol>: <model>` line showing the route.

## Claude models on OpenRouter

`openai-responses` is the OpenRouter default for every model. An `anthropic/*` model on it loses
Anthropic prompt caching and can cost several times more, so the agent prints a warning. Use
`--protocol anthropic-messages` for Claude models.

## Keys and URLs

- The `OPENROUTER_API_KEY` variable, `--api-token` or `--api-token-from-1password` supplies the
  key.
- `OPENROUTER_BASE_URL` overrides the root URL (without `/v1`).
- The OpenRouter route ignores `ANTHROPIC_*` from the environment, so neither key goes to the
  wrong service.

## Pinning

The provider, protocol and OpenRouter model are pinned in `provider.json` next to the session
database when the session first starts, because stored replies only replay under the protocol that
wrote them.

- Changing any of them needs a new `--session`, and a mismatched flag is an error.
- Sessions that predate this feature stay `anthropic`.

## Limits on OpenRouter

- Server-side compaction exists only on the direct `anthropic` provider, so a long session fails at
  the model's context limit.
- `anthropic-messages` via OpenRouter also leaves out server-side fallbacks, and sends thinking
  block binding only to Claude models, because OpenRouter rejects it on other models.
- `--effort` is sent to `openai-responses` only when set, and a value the model doesn't support
  fails with the provider's error.

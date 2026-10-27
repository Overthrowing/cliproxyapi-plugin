<img src="assets/icon.svg" width="64" height="64" alt="">

# litellm-quota

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that shows a LiteLLM virtual key's budget on the management panel's **Quota** page, next to Codex and Claude accounts.

It declares only CLIProxyAPI's `quota_provider` plugin capability. It does not modify CLIProxyAPI, and it does not take part in routing, requests, or models. When a quota card is refreshed, it calls LiteLLM's `GET /key/info` and returns:

| Card field | LiteLLM `info` field |
| --- | --- |
| Plan | `key_alias` |
| Spent | `spend` (USD) |
| Budget, Remaining | `max_budget`, `max_budget - spend` (omitted when `max_budget` is null) |
| Meter: % left, reset time | `(max_budget - spend) / max_budget`, `budget_reset_at` |
| Meter label | `budget_duration` |

Budget reset is not supported; LiteLLM resets budgets on its own schedule.

## Build

Requires Go 1.26 with cgo. Build on the same OS and architecture as the CLIProxyAPI server.

```sh
make build   # build/plugins/<os>/<arch>/litellm-quota.<dylib|so|dll>
make test
```

Without a local Go install: `nix shell nixpkgs#go nixpkgs#gnumake -c make build`.

For the official Docker image (Debian, glibc), `make linux` builds the `.so` in a `golang:1.26-bookworm` container. On an Apple Silicon host this produces `linux/arm64`; pass `GOARCH=amd64` for an x86-64 server.

## Install

1. Copy the library into CLIProxyAPI's plugin directory as `<plugins.dir>/<os>/<arch>/litellm-quota.<ext>` (or run `make install PLUGINS_DIR=...`). The file name is the plugin ID. A relative `plugins.dir` resolves against CLIProxyAPI's working directory.

2. Enable it in CLIProxyAPI's `config.yaml`. The `plugins` block is the same in legacy and v8 layouts:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       litellm-quota:
         enabled: true
   ```

3. Add one credential file per key to CLIProxyAPI's auth directory (`oauth.auth-dir`, legacy `auth-dir`), based on [`examples/litellm-quota.json`](examples/litellm-quota.json):

   ```json
   {
     "type": "litellm-quota",
     "base_url": "https://litellm.example.com",
     "api_key": "sk-..."
   }
   ```

   `base_url` is the gateway URL. Both the root and the OpenAI-style `/v1` base work, because `/key/info` is served from the root. The file name is the card title.

4. Restart CLIProxyAPI, open **Quota**, and refresh the card. It appears under **All**, after the built-in providers, and in the plugin tab.

## Why a separate credential file

The Quota page lists only credentials stored as files in the auth directory. API keys in `config.yaml` (`api-keys.claude`, `openai-compatibility`, and so on) never appear there, and CLIProxyAPI doesn't expose them to plugins. The `litellm-quota` file gives the page an entry to show. If you also route requests through LiteLLM from `config.yaml`, the key is stored in both places; update both when you rotate it.

The file is loaded as a credential with no models, so it is never selected for requests. Don't name an `openai-compatibility` entry `litellm-quota`: that entry's models would attach to this file, which has no upstream configuration.

## Notes

- The plugin calls LiteLLM directly. It honours `HTTPS_PROXY`/`HTTP_PROXY` environment variables but not CLIProxyAPI's `proxy-url`.
- Error messages from LiteLLM appear on the card. The key is redacted from them.
- CLIProxyAPI loads plugins as native code inside its process. The plugin recovers its own panics so that a bad response can't crash the server.
- Tested against CLIProxyAPI v8.0.45 (`main` at `ad8bdd3`) and the management panel's generic plugin quota card.

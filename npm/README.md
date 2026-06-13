# @mininglamp-oss/octo-daemon

Local agent runtime reporter for the [OCTO](https://github.com/Mininglamp-OSS) platform. Detects installed AI agent CLIs (Claude Code, Codex, OpenClaw, Hermes) and reports their status to your OCTO server.

## Install

```bash
npm install -g @mininglamp-oss/octo-daemon
```

The matching prebuilt Go binary ships inside a platform sub-package (`@mininglamp-oss/octo-daemon-<os>-<cpu>`) selected automatically by npm — there is no postinstall download, so registry mirrors work transparently.

The `octo-daemon` command goes on your PATH automatically (npm symlinks it into its global bin dir — no manual PATH editing). Verify:

```bash
octo-daemon version
```

If you get `octo-daemon: command not found`, npm's global bin dir is not on your PATH (common with nvm / a custom prefix) — print it with `echo "$(npm config get prefix)/bin"` and add it to `PATH`.

## Use

In OCTO, send `/daemon` to BotFather to receive your API key, then:

```bash
# Foreground run (blocks; good for a first run to watch it register):
octo-daemon start --api-key "uk_xxx" --api-url "http://your-server:8090"

# Or run it as a persistent background service (auto-starts at login):
octo-daemon service install
octo-daemon status
```

A single-host deployment needs only `--api-key` and `--api-url` (BotFather's `/daemon` reply gives you both). **Split-service deployments** also set `OCTO_FLEET_URL` / `OCTO_SERVER_URL` (both default to `--api-url`); these and other env vars are documented in the [full README](https://github.com/Mininglamp-OSS/octo-daemon-cli#-environment-variables).

Full documentation: <https://github.com/Mininglamp-OSS/octo-daemon-cli>

## Supported platforms

darwin / linux on x64 and arm64. Other platforms (including Windows): build from source with `make build`.

## License

Apache-2.0

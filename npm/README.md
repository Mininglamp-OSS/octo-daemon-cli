# @mininglamp-oss/octo-daemon

Local agent runtime reporter for the [OCTO](https://github.com/Mininglamp-OSS) platform. Detects installed AI agent CLIs (Claude Code, OpenClaw) and reports their status to your OCTO server.

## Install

```bash
npm install -g @mininglamp-oss/octo-daemon
```

The daemon's own prebuilt Go binary ships inside a platform sub-package (`@mininglamp-oss/octo-daemon-<os>-<cpu>`) selected automatically by npm — the daemon binary itself has no postinstall download.

> **Bundled `octo-cli` — networked install (intentional).** This package depends on `@mininglamp-oss/octo-cli` so the CLI is installed alongside the daemon. Unlike the daemon binary, octo-cli pulls its binary via a **postinstall download**, so `npm install -g` reaches the network for that dependency and is **not** fully mirror-transparent, and it inherits octo-cli's `os`/`cpu` matrix (darwin/linux/win32 × x64/arm64). Deliberate product decision: octo-cli is a required companion, so a failed octo-cli install fails loudly rather than silently leaving it absent. Air-gapped / mirror-only deployments must provision octo-cli's binary out-of-band.

The `octo-daemon` command goes on your PATH automatically (npm symlinks it into its global bin dir — no manual PATH editing). Verify:

```bash
octo-daemon version
```

If you get `octo-daemon: command not found`, npm's global bin dir is not on your PATH (common with nvm / a custom prefix) — print it with `echo "$(npm config get prefix)/bin"` and add it to `PATH`.

## Use

In OCTO, send `/daemon` to BotFather to receive your space ID, server / fleet URLs and API key, then configure a space and start:

```bash
# Configure a space (idempotent by --space-id; repeat per space for multi-space):
octo-daemon config \
  --space-id "your_space_id" \
  --server-url "http://your-server:8090" \
  --fleet-url  "http://your-server:8090" \
  --api-key    "uk_xxx"

# Start (foreground, blocks; good for a first run to watch it register):
octo-daemon start

# Or run it detached in the background:
octo-daemon start --daemon       # logs to ~/.octo-daemon/daemon.log
octo-daemon status
```

`config` writes one profile per space into `~/.octo-daemon/config.json`; `start`
supervises all of them in a single process. **Split-service deployments** point
`--fleet-url` and `--server-url` at different addresses. Full env/config details
are in the [full README](https://github.com/Mininglamp-OSS/octo-daemon-cli#%EF%B8%8F-configuration--environment).

Full documentation: <https://github.com/Mininglamp-OSS/octo-daemon-cli>

## Supported platforms

darwin / linux on x64 and arm64. Other platforms (including Windows): build from source with `make build`.

## License

Apache-2.0

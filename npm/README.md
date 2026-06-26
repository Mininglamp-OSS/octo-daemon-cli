# @mininglamp-oss/octo-daemon

Local agent runtime reporter for the [OCTO](https://github.com/Mininglamp-OSS) platform. Detects installed AI agent CLIs (Claude Code, OpenClaw) and reports their status to your OCTO server.

## Install

```bash
npm install -g @mininglamp-oss/octo-daemon
```

The daemon's own prebuilt Go binary ships inside a platform sub-package (`@mininglamp-oss/octo-daemon-<os>-<cpu>`) selected automatically by npm.

> Installing also pulls in `@mininglamp-oss/octo-cli`, which downloads its binary on install — so `npm install -g` needs network access (a registry mirror alone is not enough); air-gapped installs must provide octo-cli's binary separately.

The `octo-daemon` command goes on your PATH automatically (npm symlinks it into its global bin dir — no manual PATH editing). Verify:

```bash
octo-daemon version
```

If you get `octo-daemon: command not found`, npm's global bin dir is not on your PATH (common with nvm / a custom prefix) — print it with `echo "$(npm config get prefix)/bin"` and add it to `PATH`.

## Use

In OCTO, send `/daemon` to BotFather to receive your server URL and API key, then configure a space and start:

```bash
# Configure a space (idempotent by the verified space_id; repeat per API key for multi-space):
octo-daemon config \
  --server-url "http://your-server:8090" \
  --api-key    "uk_xxx"

# Run in the foreground (blocks; good for a first run to watch it register):
octo-daemon run

# Or manage the background service with pm2 through the npm shim:
octo-daemon start
octo-daemon stop
octo-daemon restart
octo-daemon logs
octo-daemon status
octo-daemon status --json
octo-daemon service status
```

`config` writes one profile per space into `~/.octo-daemon/config.json`; `run`
serves all profiles in a single foreground process. `start`, `stop`, `restart`,
`logs`, and `service ...` are npm shim commands because pm2 service management
belongs to the Node.js layer. The shim writes `~/.octo-daemon/ecosystem.config.js`
and asks pm2 to execute the resolved Go binary as `run --config <path>`.

Command ownership is explicit: `config`, `run`, `status`, `upgrade`, and
`version` are native Go binary commands; `start`, `stop`, `restart`, `logs`, and
`service install/status/remove` are npm shim commands for the pm2 service.

**Split-service deployments** point `--fleet-url` and `--server-url` at different
addresses. Full env/config details are in the [full README](https://github.com/Mininglamp-OSS/octo-daemon-cli#%EF%B8%8F-configuration--environment).

Full documentation: <https://github.com/Mininglamp-OSS/octo-daemon-cli>

## Supported platforms

darwin / linux on x64 and arm64. Other platforms (including Windows): build from source with `make build`.

## License

Apache-2.0

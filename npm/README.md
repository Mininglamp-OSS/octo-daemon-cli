# @mininglamp-oss/octo-daemon

Local agent runtime reporter for the [OCTO](https://github.com/Mininglamp-OSS) platform. Detects installed AI agent CLIs (Claude Code, Codex, OpenClaw, Hermes) and reports their status to your OCTO server.

## Install

```bash
npm install -g @mininglamp-oss/octo-daemon
```

The matching prebuilt Go binary ships inside a platform sub-package (`@mininglamp-oss/octo-daemon-<os>-<cpu>`) selected automatically by npm — there is no postinstall download, so registry mirrors work transparently.

## Use

In OCTO, send `/daemon` to BotFather to receive your API key, then:

```bash
octo-daemon start --api-key "uk_xxx" --api-url "http://your-server:8090"
octo-daemon service install   # recommended: register as launchd / systemd --user service
octo-daemon status
```

Full documentation: <https://github.com/Mininglamp-OSS/octo-daemon-cli>

## Supported platforms

darwin / linux / win32 on x64 and arm64. Other platforms: build from source with `make build`.

## License

Apache-2.0

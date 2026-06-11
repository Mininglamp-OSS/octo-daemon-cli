#!/usr/bin/env node
"use strict";

// Thin shim: resolve the prebuilt Go binary from the platform sub-package
// that npm selected via this package's optionalDependencies (os/cpu
// constrained), then forward all args and stdio to it.
//
// There is deliberately NO postinstall download here: every byte comes from
// the npm registry itself (platform sub-packages carry the binary inside
// their tarball), so registry mirrors work transparently and npm's own
// integrity checks replace any custom checksum logic.

const os = require("os");
const { spawnSync } = require("child_process");

const PLATFORM_PACKAGES = {
  "darwin-arm64": "@mininglamp-oss/octo-daemon-darwin-arm64",
  "darwin-x64": "@mininglamp-oss/octo-daemon-darwin-x64",
  "linux-arm64": "@mininglamp-oss/octo-daemon-linux-arm64",
  "linux-x64": "@mininglamp-oss/octo-daemon-linux-x64",
  "win32-arm64": "@mininglamp-oss/octo-daemon-win32-arm64",
  "win32-x64": "@mininglamp-oss/octo-daemon-win32-x64",
};

const isWin = process.platform === "win32";
const BIN_NAME = isWin ? "octo-daemon.exe" : "octo-daemon";

function resolveBinary() {
  const key = `${process.platform}-${process.arch}`;
  const pkg = PLATFORM_PACKAGES[key];
  if (!pkg) {
    console.error(`[octo-daemon] unsupported platform: ${key}`);
    console.error(
      "[octo-daemon] Prebuilt binaries cover darwin/linux/win32 on x64/arm64. " +
        "Build from source instead: https://github.com/Mininglamp-OSS/octo-daemon-cli",
    );
    process.exit(1);
  }
  try {
    return require.resolve(`${pkg}/bin/${BIN_NAME}`);
  } catch (_err) {
    console.error(`[octo-daemon] platform package ${pkg} is not installed.`);
    console.error(
      "[octo-daemon] npm skips optionalDependencies when installed with " +
        "--no-optional / --omit=optional, and some package managers need a " +
        "lockfile refresh after a platform change.\n" +
        "[octo-daemon] Try reinstalling: npm install -g @mininglamp-oss/octo-daemon",
    );
    process.exit(1);
  }
}

const res = spawnSync(resolveBinary(), process.argv.slice(2), { stdio: "inherit" });

if (res.error) {
  console.error(`[octo-daemon] ${res.error.message}`);
  process.exit(1);
}

// POSIX-only path (no-op on win32, where spawnSync does not surface
// signals): re-raise terminating signals so the shell observes the
// conventional 128+signum exit code; for default-ignored signals
// (SIGPIPE, ...) the explicit exit below sets the code instead.
if (res.signal) {
  process.kill(process.pid, res.signal);
  const signum = (os.constants && os.constants.signals && os.constants.signals[res.signal]) || 0;
  process.exit(128 + signum);
}

process.exit(res.status === null ? 1 : res.status);

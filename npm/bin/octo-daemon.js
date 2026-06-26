#!/usr/bin/env node
"use strict";

// Thin shim: resolve the prebuilt Go binary from the platform sub-package
// that npm selected via this package's optionalDependencies (os/cpu
// constrained). Service lifecycle commands are handled here because they are
// pm2 / Node.js operations; all daemon/business commands are forwarded to the
// Go binary.
//
// The daemon binary has deliberately NO postinstall download: every byte
// comes from the npm registry itself (platform sub-packages carry the binary
// inside their tarball), and npm's own integrity checks replace any custom
// checksum logic. (The bundled octo-cli dependency does download its binary
// via postinstall — see README.)
//
// The main package intentionally carries no os/cpu constraints — on a
// platform with no prebuilt sub-package the install succeeds and THIS shim
// emits the build-from-source pointer, instead of npm aborting the whole
// install with an opaque EBADPLATFORM.

const os = require("os");
const path = require("path");
const fs = require("fs");
const { spawnSync } = require("child_process");

const PM2_APP_NAME = "octo-daemon";
const PM2_JLIST_MAX_BUFFER = 10 * 1024 * 1024;

const PLATFORM_PACKAGES = {
  "darwin-arm64": "@mininglamp-oss/octo-daemon-darwin-arm64",
  "darwin-x64": "@mininglamp-oss/octo-daemon-darwin-x64",
  "linux-arm64": "@mininglamp-oss/octo-daemon-linux-arm64",
  "linux-x64": "@mininglamp-oss/octo-daemon-linux-x64",
};

function resolveBinary(options = {}) {
  const optional = options.optional === true;
  const fail = (lines) => {
    if (optional) return { ok: false, error: lines.join("\n") };
    for (const line of lines) console.error(line);
    process.exit(1);
  };

  const key = `${process.platform}-${process.arch}`;
  const pkg = PLATFORM_PACKAGES[key];
  if (!pkg) {
    return fail([
      `[octo-daemon] no prebuilt binary for ${key}.`,
      "[octo-daemon] Prebuilt binaries cover darwin/linux on x64/arm64. " +
        "Build from source instead: https://github.com/Mininglamp-OSS/octo-daemon-cli",
    ]);
  }
  try {
    const bin = require.resolve(`${pkg}/bin/octo-daemon`);
    return optional ? { ok: true, path: bin } : bin;
  } catch (_err) {
    return fail([
      `[octo-daemon] platform package ${pkg} is not installed.`,
      "[octo-daemon] npm skips optionalDependencies when installed with " +
        "--no-optional / --omit=optional, and some package managers need a " +
        "lockfile refresh after a platform change.\n" +
        "[octo-daemon] Try reinstalling: npm install -g @mininglamp-oss/octo-daemon",
    ]);
  }
}

function dataDir() {
  return path.join(os.homedir(), ".octo-daemon");
}

function configFilePath() {
  return path.join(dataDir(), "config.json");
}

function ecosystemPath() {
  return path.join(dataDir(), "ecosystem.config.js");
}

function usageError(message) {
  console.error(`[octo-daemon] ${message}`);
  process.exit(2);
}

function isHelpArg(arg) {
  return arg === "--help" || arg === "-h";
}

function isShimCommand(cmd) {
  return cmd === "start" || cmd === "stop" || cmd === "restart" || cmd === "logs" || cmd === "service";
}

function printRootHelp() {
  console.log(`octo-daemon - Octo Agent Runtime Daemon

Usage:
  octo-daemon <command> [options]

Native daemon commands (Go binary):
  config                         Configure one space profile
  run [--config <path>]          Run the daemon in the foreground
  status [--json]                Show process/version/profile status
  upgrade                        Upgrade the installed binary
  version                        Show binary version

Service control commands (npm shim / pm2):
  start [--config <path>]        Start/install the pm2-managed service
  stop                           Stop the pm2-managed service
  restart                        Restart the pm2-managed service
  logs [pm2 log options...]      Stream pm2 logs for the service
  service install [--config <path>]
  service status
  service remove

Native command help:
  octo-daemon help config
  octo-daemon run --help

Service command help:
  octo-daemon start --help
  octo-daemon service --help
`);
}

function printServiceHelp() {
  console.log(`octo-daemon service control (npm shim / pm2)

Usage:
  octo-daemon start [--config <path>]
  octo-daemon stop
  octo-daemon restart
  octo-daemon logs [pm2 log options...]
  octo-daemon service install [--config <path>]
  octo-daemon service status
  octo-daemon service remove

The npm shim owns these commands because pm2 is a Node.js service manager.
They manage a pm2 app named "octo-daemon" that executes the Go binary as:

  octo-daemon run --config <path>
`);
}

function parseConfigArg(args) {
  let cfgPath = "";
  const rest = [];
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];
    if (arg === "--config") {
      if (i + 1 >= args.length) usageError("--config requires a path");
      cfgPath = args[i + 1];
      i += 1;
    } else if (arg.startsWith("--config=")) {
      cfgPath = arg.slice("--config=".length);
    } else {
      rest.push(arg);
    }
  }
  if (rest.length > 0) {
    usageError(`unsupported service argument(s): ${rest.join(" ")}`);
  }
  return path.resolve(cfgPath || configFilePath());
}

function commandExists(name) {
  const probe = process.platform === "win32" ? "where" : "command";
  const args = process.platform === "win32" ? [name] : ["-v", name];
  return spawnSync(probe, args, { stdio: "ignore", shell: process.platform !== "win32" }).status === 0;
}

function ensurePM2() {
  if (commandExists("pm2")) return;
  if (!commandExists("npm")) {
    console.error("[octo-daemon] pm2 not found and npm is unavailable. Install Node.js/npm, then retry.");
    process.exit(2);
  }
  console.log("[octo-daemon] pm2 not found; installing globally via `npm install -g pm2`...");
  run("npm", ["install", "-g", "pm2"]);
}

function run(command, args, options = {}) {
  const res = spawnSync(command, args, {
    stdio: options.capture ? ["ignore", "pipe", "pipe"] : "inherit",
    encoding: options.capture ? "utf8" : undefined,
  });
  if (res.error) {
    console.error(`[octo-daemon] ${res.error.message}`);
    process.exit(1);
  }
  if (res.signal) {
    exitFromSignal(res.signal);
  }
  if (res.status !== 0) {
    if (options.capture && res.stderr) process.stderr.write(res.stderr);
    process.exit(res.status === null ? 1 : res.status);
  }
  return res;
}

function pm2AppStatus() {
  if (!commandExists("pm2")) {
    return { ok: true, found: false, online: false, pid: 0, status: "missing-pm2" };
  }
  const res = spawnSync("pm2", ["jlist"], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: PM2_JLIST_MAX_BUFFER,
  });
  if (res.error) {
    return {
      ok: false,
      found: false,
      online: false,
      pid: 0,
      status: "unknown",
      error: `[octo-daemon] failed to query pm2: ${res.error.message}`,
    };
  }
  if (res.signal) {
    return {
      ok: false,
      found: false,
      online: false,
      pid: 0,
      status: "unknown",
      error: `[octo-daemon] pm2 jlist terminated by ${res.signal}`,
    };
  }
  if (res.status !== 0 || !res.stdout) {
    const detail = res.stderr ? `: ${res.stderr.trim()}` : "";
    return {
      ok: false,
      found: false,
      online: false,
      pid: 0,
      status: "unknown",
      error: `[octo-daemon] failed to query pm2 service list${detail}`,
    };
  }
  try {
    const apps = JSON.parse(res.stdout);
    if (!Array.isArray(apps)) {
      return {
        ok: false,
        found: false,
        online: false,
        pid: 0,
        status: "unknown",
        error: "[octo-daemon] failed to parse pm2 service list.",
      };
    }
    const app = apps.find((x) => x && x.name === PM2_APP_NAME);
    if (!app) return { ok: true, found: false, online: false, pid: 0, status: "not-installed" };
    const status = app.pm2_env && app.pm2_env.status ? String(app.pm2_env.status) : "unknown";
    return {
      ok: true,
      found: true,
      online: status === "online",
      pid: Number(app.pid || 0),
      status,
    };
  } catch (_err) {
    return {
      ok: false,
      found: false,
      online: false,
      pid: 0,
      status: "unknown",
      error: "[octo-daemon] failed to parse pm2 service list.",
    };
  }
}

function daemonStatus() {
  const bin = resolveBinary({ optional: true });
  if (!bin.ok) {
    return { ok: false, locked: false, pid: 0, error: bin.error };
  }

  const res = spawnSync(bin.path, ["status", "--json"], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (res.error) {
    return { ok: false, locked: false, pid: 0, error: `[octo-daemon] ${res.error.message}` };
  }
  if (res.signal) {
    return {
      ok: false,
      locked: false,
      pid: 0,
      error: `[octo-daemon] status probe terminated by ${res.signal}`,
    };
  }
  if (res.status !== 0) {
    const detail = res.stderr ? `: ${res.stderr.trim()}` : "";
    return {
      ok: false,
      locked: false,
      pid: 0,
      error: `[octo-daemon] status probe failed with exit ${res.status === null ? 1 : res.status}${detail}`,
    };
  }
  try {
    const status = JSON.parse(res.stdout);
    return {
      ok: true,
      locked: status && status.locked === true,
      pid: Number(status && status.pid ? status.pid : 0),
    };
  } catch (_err) {
    return {
      ok: false,
      locked: false,
      pid: 0,
      error: "[octo-daemon] failed to parse `octo-daemon status --json` output.",
    };
  }
}

function pm2AppCanOwnLock(app, pid) {
  if (!app || !app.ok || !app.found || !pid) return false;
  if (app.pid === pid) return true;
  if (app.online) return false;
  return app.status !== "stopped";
}

function foregroundDaemon(app) {
  const status = daemonStatus();
  if (!status.ok) {
    return {
      active: false,
      pid: 0,
      unknown: true,
      error: status.error,
    };
  }
  if (!status.locked) return { active: false, pid: 0 };
  const pid = status.pid;
  if (pm2AppCanOwnLock(app, pid)) return { active: false, pid: 0 };
  return { active: true, pid };
}

function assertNoForegroundDaemon(app) {
  const foreground = foregroundDaemon(app);
  if (foreground.unknown) {
    console.error(foreground.error);
    console.error("[octo-daemon] unable to verify whether a foreground daemon is already running.");
    process.exit(1);
  }
  if (!foreground.active) return;
  const pid = foreground.pid ? `pid ${foreground.pid}` : "pid unknown";
  console.error(`[octo-daemon] daemon is already running in foreground mode (${pid}).`);
  console.error("[octo-daemon] Stop that `octo-daemon run` process before starting the background service.");
  process.exit(2);
}

function warnUnknownProbe(prefix, probe) {
  if (probe && probe.error) {
    console.error(`${prefix}: ${probe.error}`);
  } else {
    console.error(`${prefix}: unknown probe failure.`);
  }
}

function writeEcosystem(goBin, cfgPath) {
  fs.mkdirSync(dataDir(), { recursive: true, mode: 0o755 });
  const content = `// Generated by \`octo-daemon start\`. Do not edit by hand.
module.exports = {
  apps: [{
    name: ${JSON.stringify(PM2_APP_NAME)},
    script: ${JSON.stringify(goBin)},
    interpreter: "none",
    args: ${JSON.stringify(["run", "--config", cfgPath])},
    autorestart: true,
    stop_exit_codes: [2, 78],
    max_restarts: 10,
    restart_delay: 2000,
    kill_timeout: 5000,
  }]
};
`;
  fs.writeFileSync(ecosystemPath(), content, { mode: 0o644 });
  return ecosystemPath();
}

function serviceStart(args) {
  const app = pm2AppStatus();
  assertNoForegroundDaemon(app);
  const goBin = resolveBinary();
  const cfgPath = parseConfigArg(args);
  const ecoPath = writeEcosystem(goBin, cfgPath);
  console.log(`[octo-daemon] wrote pm2 ecosystem: ${ecoPath}`);
  ensurePM2();
  run("pm2", ["startOrRestart", ecoPath]);
  run("pm2", ["save"]);
  console.log(`[octo-daemon] service started via pm2 (app ${JSON.stringify(PM2_APP_NAME)}).`);
}

function serviceStop() {
  const app = pm2AppStatus();
  const foreground = foregroundDaemon(app);
  if (foreground.unknown) {
    warnUnknownProbe("[octo-daemon] warning: continuing stop without daemon lock status", foreground);
  }
  if (foreground.active) {
    const pid = foreground.pid ? `pid ${foreground.pid}` : "pid unknown";
    console.error(`[octo-daemon] daemon is running in foreground mode (${pid}); stop the terminal running \`octo-daemon run\`.`);
    process.exit(2);
  }
  if (app.ok && !app.found) {
    console.log("[octo-daemon] service is not installed.");
    return;
  }
  if (!app.ok) {
    warnUnknownProbe("[octo-daemon] warning: continuing stop without pm2 service status", app);
  }
  ensurePM2();
  if (app.ok && app.status === "stopped") {
    console.log("[octo-daemon] service is already stopped.");
    return;
  }
  run("pm2", ["stop", PM2_APP_NAME]);
  run("pm2", ["save"]);
  console.log(`[octo-daemon] service stopped (app ${JSON.stringify(PM2_APP_NAME)}).`);
}

function serviceRestart() {
  let app = pm2AppStatus();
  assertNoForegroundDaemon(app);
  if (app.ok && !app.found) {
    console.error("[octo-daemon] service is not installed. Run `octo-daemon start` first.");
    process.exit(2);
  }
  if (!app.ok) {
    warnUnknownProbe("[octo-daemon] warning: continuing restart without pm2 service status", app);
  }
  ensurePM2();
  if (!app.ok) {
    app = pm2AppStatus();
    if (app.ok && !app.found) {
      console.error("[octo-daemon] service is not installed. Run `octo-daemon start` first.");
      process.exit(2);
    }
    if (!app.ok) {
      warnUnknownProbe("[octo-daemon] warning: continuing restart without confirmed pm2 service status", app);
    }
  }
  run("pm2", ["restart", PM2_APP_NAME]);
  run("pm2", ["save"]);
}

function serviceLogs(args) {
  const app = pm2AppStatus();
  if (app.ok && !app.found) {
    console.error("[octo-daemon] service is not installed. Run `octo-daemon start` first.");
    process.exit(2);
  }
  if (!app.ok) {
    warnUnknownProbe("[octo-daemon] warning: continuing logs without pm2 service status", app);
  }
  ensurePM2();
  run("pm2", ["logs", PM2_APP_NAME, ...args]);
}

function serviceStatus() {
  const app = pm2AppStatus();
  if (!app.ok) {
    console.error(app.error);
    process.exit(1);
  }
  if (!app.found) {
    console.log("Service: not installed");
    return;
  }
  console.log(`Service: ${app.status}`);
  if (app.pid) console.log(`PID: ${app.pid}`);
  console.log("Supervisor: pm2");
}

function serviceRemove() {
  const app = pm2AppStatus();
  if (app.ok && !app.found) {
    console.log("[octo-daemon] service is not installed.");
    return;
  }
  if (!app.ok) {
    warnUnknownProbe("[octo-daemon] warning: continuing remove without pm2 service status", app);
  }
  ensurePM2();
  run("pm2", ["delete", PM2_APP_NAME]);
  run("pm2", ["save"]);
  console.log(`[octo-daemon] service removed from pm2 (app ${JSON.stringify(PM2_APP_NAME)}).`);
}

function exitFromSignal(signal) {
  process.kill(process.pid, signal);
  const signum = (os.constants && os.constants.signals && os.constants.signals[signal]) || 0;
  process.exit(128 + signum);
}

function runGo(args) {
  const res = spawnSync(resolveBinary(), args, { stdio: "inherit" });

  if (res.error) {
    console.error(`[octo-daemon] ${res.error.message}`);
    process.exit(1);
  }

  if (res.signal) {
    exitFromSignal(res.signal);
  }

  process.exit(res.status === null ? 1 : res.status);
}

function handleNodeCommand(args) {
  const [cmd, subcmd, ...rest] = args;
  if (!cmd) {
    printRootHelp();
    return true;
  }
  if (isHelpArg(cmd)) {
    printRootHelp();
    return true;
  }
  if (cmd === "help") {
    if (!subcmd) {
      printRootHelp();
      return true;
    }
    if (isShimCommand(subcmd)) {
      printServiceHelp();
      return true;
    }
    return false;
  }
  if (cmd === "service" && (isHelpArg(subcmd) || subcmd === "help")) {
    printServiceHelp();
    return true;
  }
  if (cmd === "start") {
    if (args.slice(1).some(isHelpArg)) {
      printServiceHelp();
      return true;
    }
    serviceStart(args.slice(1));
    return true;
  }
  if (cmd === "stop") {
    if (isHelpArg(subcmd)) {
      printServiceHelp();
      return true;
    }
    if (args.length > 1) usageError("stop does not accept arguments");
    serviceStop();
    return true;
  }
  if (cmd === "restart") {
    if (isHelpArg(subcmd)) {
      printServiceHelp();
      return true;
    }
    if (args.length > 1) usageError("restart does not accept arguments");
    serviceRestart();
    return true;
  }
  if (cmd === "logs") {
    if (isHelpArg(subcmd)) {
      printServiceHelp();
      return true;
    }
    serviceLogs(args.slice(1));
    return true;
  }
  if (cmd === "service") {
    if (subcmd === "install") {
      if (rest.some(isHelpArg)) {
        printServiceHelp();
      } else {
        serviceStart(rest);
      }
    } else if (subcmd === "status") {
      if (rest.length === 1 && isHelpArg(rest[0])) {
        printServiceHelp();
        return true;
      }
      if (rest.length > 0) usageError("service status does not accept arguments");
      serviceStatus();
    } else if (subcmd === "remove") {
      if (rest.length === 1 && isHelpArg(rest[0])) {
        printServiceHelp();
        return true;
      }
      if (rest.length > 0) usageError("service remove does not accept arguments");
      serviceRemove();
    } else {
      usageError("service requires one of: install, status, remove");
    }
    return true;
  }
  return false;
}

function forwardToGo(args) {
  runGo(args);
}

const args = process.argv.slice(2);
if (!handleNodeCommand(args)) {
  forwardToGo(args);
}

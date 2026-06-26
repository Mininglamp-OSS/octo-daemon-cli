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

const PLATFORM_PACKAGES = {
  "darwin-arm64": "@mininglamp-oss/octo-daemon-darwin-arm64",
  "darwin-x64": "@mininglamp-oss/octo-daemon-darwin-x64",
  "linux-arm64": "@mininglamp-oss/octo-daemon-linux-arm64",
  "linux-x64": "@mininglamp-oss/octo-daemon-linux-x64",
};

function resolveBinary() {
  const key = `${process.platform}-${process.arch}`;
  const pkg = PLATFORM_PACKAGES[key];
  if (!pkg) {
    console.error(`[octo-daemon] no prebuilt binary for ${key}.`);
    console.error(
      "[octo-daemon] Prebuilt binaries cover darwin/linux on x64/arm64. " +
        "Build from source instead: https://github.com/Mininglamp-OSS/octo-daemon-cli",
    );
    process.exit(1);
  }
  try {
    return require.resolve(`${pkg}/bin/octo-daemon`);
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

function dataDir() {
  return path.join(os.homedir(), ".octo-daemon");
}

function configFilePath() {
  return path.join(dataDir(), "config.json");
}

function ecosystemPath() {
  return path.join(dataDir(), "ecosystem.config.js");
}

function pidFilePath() {
  return path.join(dataDir(), "daemon.pid");
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
  status                         Show process/version/profile status
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
    return { found: false, online: false, pid: 0, status: "missing-pm2" };
  }
  const res = spawnSync("pm2", ["jlist"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
  if (res.status !== 0 || !res.stdout) {
    return { found: false, online: false, pid: 0, status: "unknown" };
  }
  try {
    const apps = JSON.parse(res.stdout);
    const app = apps.find((x) => x && x.name === PM2_APP_NAME);
    if (!app) return { found: false, online: false, pid: 0, status: "not-installed" };
    const status = app.pm2_env && app.pm2_env.status ? String(app.pm2_env.status) : "unknown";
    return {
      found: true,
      online: status === "online",
      pid: Number(app.pid || 0),
      status,
    };
  } catch (_err) {
    return { found: false, online: false, pid: 0, status: "unknown" };
  }
}

function readDaemonPid() {
  try {
    const raw = fs.readFileSync(pidFilePath(), "utf8").trim();
    const pid = Number.parseInt(raw, 10);
    return Number.isFinite(pid) && pid > 0 ? pid : 0;
  } catch (_err) {
    return 0;
  }
}

function pidIsAlive(pid) {
  if (!pid) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err && err.code === "EPERM";
  }
}

function foregroundDaemonPid(app) {
  const pid = readDaemonPid();
  if (!pid || !pidIsAlive(pid)) return 0;
  if (app && app.online && app.pid === pid) return 0;
  return pid;
}

function assertNoForegroundDaemon() {
  const app = pm2AppStatus();
  const pid = foregroundDaemonPid(app);
  if (!pid) return;
  console.error(`[octo-daemon] daemon is already running in foreground mode (pid ${pid}).`);
  console.error("[octo-daemon] Stop that `octo-daemon run` process before starting the background service.");
  process.exit(2);
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
  assertNoForegroundDaemon();
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
  const foregroundPid = foregroundDaemonPid(app);
  if (foregroundPid) {
    console.error(`[octo-daemon] daemon is running in foreground mode (pid ${foregroundPid}); stop the terminal running \`octo-daemon run\`.`);
    process.exit(2);
  }
  if (!app.found) {
    console.log("[octo-daemon] service is not installed.");
    return;
  }
  ensurePM2();
  if (app.status === "stopped") {
    console.log("[octo-daemon] service is already stopped.");
  } else {
    run("pm2", ["stop", PM2_APP_NAME]);
  }
  run("pm2", ["save"]);
  console.log(`[octo-daemon] service stopped (app ${JSON.stringify(PM2_APP_NAME)}).`);
}

function serviceRestart() {
  const app = pm2AppStatus();
  if (!app.found) {
    console.error("[octo-daemon] service is not installed. Run `octo-daemon start` first.");
    process.exit(2);
  }
  ensurePM2();
  run("pm2", ["restart", PM2_APP_NAME]);
  run("pm2", ["save"]);
}

function serviceLogs(args) {
  const app = pm2AppStatus();
  if (!app.found) {
    console.error("[octo-daemon] service is not installed. Run `octo-daemon start` first.");
    process.exit(2);
  }
  ensurePM2();
  run("pm2", ["logs", PM2_APP_NAME, ...args]);
}

function serviceStatus() {
  const app = pm2AppStatus();
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
  if (!app.found) {
    console.log("[octo-daemon] service is not installed.");
    return;
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

function runGo(args, options = {}) {
  const res = spawnSync(resolveBinary(), args, { stdio: "inherit" });

  if (res.error) {
    console.error(`[octo-daemon] ${res.error.message}`);
    process.exit(1);
  }

  if (res.signal) {
    exitFromSignal(res.signal);
  }

  const status = res.status === null ? 1 : res.status;
  if (!options.returnStatus) {
    process.exit(status);
  }
  return status;
}

function handleNodeCommand(args) {
  const [cmd, subcmd, ...rest] = args;
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
    if (isHelpArg(subcmd)) {
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
      if (rest.length === 1 && isHelpArg(rest[0])) {
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

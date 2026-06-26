"use strict";

// Tests for prepare-packages.js and the bin shim: build a fake GoReleaser
// dist/ layout, run the script, assert the emitted package tree, verify the
// npm matrix matches .goreleaser.yaml, and spawn a real binary through the
// shim. Runs with `node --test` — no dependencies.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const { execFileSync, spawnSync } = require("child_process");

const SCRIPT = path.join(__dirname, "prepare-packages.js");
const SHIM = path.join(__dirname, "..", "bin", "octo-daemon.js");
const GORELEASER_YAML = path.join(__dirname, "..", "..", ".goreleaser.yaml");
const { PLATFORMS } = require(SCRIPT);
const VERSION = "9.9.9";

function makeDist(distDir) {
  fs.mkdirSync(distDir, { recursive: true });
  for (const { goOs, goArch } of PLATFORMS) {
    const stage = fs.mkdtempSync(path.join(os.tmpdir(), "stage-"));
    fs.writeFileSync(
      path.join(stage, "octo-daemon"),
      `#!/bin/sh
if [ "$1" = "status" ] && [ "$2" = "--json" ]; then
  printf '%s\\n' '{"status":"stopped","locked":false,"pid":0,"pid_file_stale":false}'
  exit 0
fi
echo fake ${goOs}/${goArch} "$@"
`,
      {
        mode: 0o755,
      },
    );
    const archive = path.join(distDir, `octo-daemon_${VERSION}_${goOs}_${goArch}.tar.gz`);
    execFileSync("tar", ["-czf", archive, "-C", stage, "octo-daemon"]);
    fs.rmSync(stage, { recursive: true, force: true });
  }
}

function run(args) {
  return spawnSync(process.execPath, [SCRIPT, ...args], { encoding: "utf8" });
}

function shellQuote(s) {
  return `'${String(s).replaceAll("'", "'\\''")}'`;
}

function hostPlatformKey(t) {
  const key = `${process.platform}-${process.arch}`;
  const supported = new Set(PLATFORMS.map((p) => `${p.npmOs}-${p.npmCpu}`));
  if (!supported.has(key)) {
    t.skip(`host ${key} not in the platform matrix`);
    return "";
  }
  return key;
}

function installFakePlatformBinary(t, mainPackageDir, script) {
  const key = hostPlatformKey(t);
  if (!key) return "";
  const binDir = path.join(
    mainPackageDir,
    "node_modules",
    "@mininglamp-oss",
    `octo-daemon-${key}`,
    "bin",
  );
  fs.mkdirSync(binDir, { recursive: true });
  const binPath = path.join(binDir, "octo-daemon");
  fs.writeFileSync(binPath, script, { mode: 0o755 });
  return binPath;
}

function fakeStatusBinary(status) {
  return `#!/bin/sh
if [ "$1" = "status" ] && [ "$2" = "--json" ]; then
  printf '%s\\n' ${shellQuote(JSON.stringify(status))}
  exit 0
fi
echo fake "$@"
`;
}

function fakeFailingStatusBinary() {
  return `#!/bin/sh
if [ "$1" = "status" ] && [ "$2" = "--json" ]; then
  printf '%s\\n' 'status unavailable' >&2
  exit 9
fi
echo fake "$@"
`;
}

test("npm matrix matches .goreleaser.yaml goos × goarch", () => {
  // Cheap structural parse: goos/goarch list items inside builds. Keeps the
  // two matrices in lockstep without a YAML dependency — adding windows (or
  // 386) to one side without the other must fail this test.
  const yaml = fs.readFileSync(GORELEASER_YAML, "utf8");
  const section = (name) => {
    const m = yaml.match(new RegExp(`${name}:\\n((?:\\s*(?:#[^\\n]*)?\\n)*(?:\\s+-\\s+\\S+\\n)+)`));
    assert.ok(m, `cannot find ${name}: list in .goreleaser.yaml`);
    return [...m[1].matchAll(/-\s+(\S+)/g)].map((x) => x[1]);
  };
  const goos = section("goos");
  const goarch = section("goarch");
  const releaser = new Set(goos.flatMap((o) => goarch.map((a) => `${o}/${a}`)));
  const npm = new Set(PLATFORMS.map((p) => `${p.goOs}/${p.goArch}`));
  assert.deepStrictEqual([...npm].sort(), [...releaser].sort());
});

test("emits one package per platform and a pinned main package", (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "prep-pkgs-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const distDir = path.join(tmp, "dist");
  const outDir = path.join(tmp, "out");
  makeDist(distDir);

  const res = run(["--version", VERSION, "--dist", distDir, "--out", outDir]);
  assert.strictEqual(res.status, 0, res.stderr);

  for (const { npmOs, npmCpu } of PLATFORMS) {
    const pkgDir = path.join(outDir, `octo-daemon-${npmOs}-${npmCpu}`);
    const manifest = JSON.parse(fs.readFileSync(path.join(pkgDir, "package.json"), "utf8"));
    assert.strictEqual(manifest.name, `@mininglamp-oss/octo-daemon-${npmOs}-${npmCpu}`);
    assert.strictEqual(manifest.version, VERSION);
    assert.deepStrictEqual(manifest.os, [npmOs]);
    assert.deepStrictEqual(manifest.cpu, [npmCpu]);
    const bin = path.join(pkgDir, "bin", "octo-daemon");
    assert.ok(fs.existsSync(bin), `missing ${bin}`);
    assert.ok(fs.statSync(bin).mode & 0o100, `${bin} not executable`);
  }

  // Main package: version stamped, optionalDependencies exact-pinned to
  // every platform package, shim shipped, dev scripts stripped, and NO
  // os/cpu constraints ON THE MAIN PACKAGE ITSELF (so the shim — not npm —
  // owns the unsupported-platform message for the daemon binary).
  //
  // NOTE: the octo-cli hard dependency DOES carry an os/cpu matrix, so
  // end-to-end "installs anywhere" no longer fully holds on platforms
  // outside octo-cli's matrix — an intentional product trade-off (octo-cli
  // is a required companion; see README). This assertion still guards the
  // main package's own constraint-free shape, which is unchanged.
  const main = JSON.parse(
    fs.readFileSync(path.join(outDir, "octo-daemon", "package.json"), "utf8"),
  );
  assert.strictEqual(main.name, "@mininglamp-oss/octo-daemon");
  assert.strictEqual(main.version, VERSION);
  assert.strictEqual(main.os, undefined);
  assert.strictEqual(main.cpu, undefined);
  assert.strictEqual(Object.keys(main.optionalDependencies).length, PLATFORMS.length);
  for (const [dep, pin] of Object.entries(main.optionalDependencies)) {
    assert.match(dep, /^@mininglamp-oss\/octo-daemon-(darwin|linux)-(x64|arm64)$/);
    assert.strictEqual(pin, VERSION, `${dep} must be exact-pinned`);
  }
  assert.strictEqual(main.scripts, undefined);
  assert.ok(fs.existsSync(path.join(outDir, "octo-daemon", "bin", "octo-daemon.js")));

  // No stray extraction workdirs left behind.
  const leftovers = fs.readdirSync(outDir).filter((n) => n.startsWith(".extract-"));
  assert.deepStrictEqual(leftovers, []);
});

test("shim resolves the platform package and propagates args + exit code", (t) => {
  const key = `${process.platform}-${process.arch}`;
  const supported = new Set(PLATFORMS.map((p) => `${p.npmOs}-${p.npmCpu}`));
  if (!supported.has(key)) {
    t.skip(`host ${key} not in the platform matrix`);
    return;
  }

  // Lay the generated platform package out as node_modules and run the
  // shim against it — this is the actual install topology npm produces.
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-e2e-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const distDir = path.join(tmp, "dist");
  const outDir = path.join(tmp, "out");
  makeDist(distDir);
  assert.strictEqual(run(["--version", VERSION, "--dist", distDir, "--out", outDir]).status, 0);

  const pkgRoot = path.join(tmp, "node_modules", "@mininglamp-oss");
  fs.mkdirSync(pkgRoot, { recursive: true });
  fs.cpSync(path.join(outDir, `octo-daemon-${key}`), path.join(pkgRoot, `octo-daemon-${key}`), {
    recursive: true,
  });
  const mainDir = path.join(pkgRoot, "octo-daemon");
  fs.cpSync(path.join(outDir, "octo-daemon"), mainDir, { recursive: true });

  const res = spawnSync(process.execPath, [path.join(mainDir, "bin", "octo-daemon.js"), "--ping"], {
    encoding: "utf8",
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.match(res.stdout, /fake (darwin|linux)\/(amd64|arm64) --ping/);

  const rootHelp = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "--help",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(rootHelp.status, 0, rootHelp.stderr);
  assert.match(rootHelp.stdout, /Native daemon commands \(Go binary\)/);
  assert.match(rootHelp.stdout, /Service control commands \(npm shim \/ pm2\)/);
  assert.doesNotMatch(rootHelp.stdout, /fake (darwin|linux)\/(amd64|arm64)/);

  const rootHelpAlias = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "help",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(rootHelpAlias.status, 0, rootHelpAlias.stderr);
  assert.match(rootHelpAlias.stdout, /octo-daemon - Octo Agent Runtime Daemon/);
  assert.doesNotMatch(rootHelpAlias.stdout, /fake (darwin|linux)\/(amd64|arm64)/);

  const rootHelpBare = spawnSync(process.execPath, [path.join(mainDir, "bin", "octo-daemon.js")], {
    encoding: "utf8",
  });
  assert.strictEqual(rootHelpBare.status, 0, rootHelpBare.stderr);
  assert.match(rootHelpBare.stdout, /Service control commands \(npm shim \/ pm2\)/);
  assert.doesNotMatch(rootHelpBare.stdout, /fake (darwin|linux)\/(amd64|arm64)/);

  const nativeHelp = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "help",
    "config",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(nativeHelp.status, 0, nativeHelp.stderr);
  assert.match(nativeHelp.stdout, /fake (darwin|linux)\/(amd64|arm64) help config/);

  const serviceHelp = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "help",
    "start",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(serviceHelp.status, 0, serviceHelp.stderr);
  assert.match(serviceHelp.stdout, /octo-daemon service control \(npm shim \/ pm2\)/);

  const serviceSubHelp = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "service",
    "status",
    "--help",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(serviceSubHelp.status, 0, serviceSubHelp.stderr);
  assert.match(serviceSubHelp.stdout, /octo-daemon service control \(npm shim \/ pm2\)/);

  const serviceInstallHelp = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "service",
    "install",
    "--config",
    path.join(os.tmpdir(), "config.json"),
    "--help",
  ], {
    encoding: "utf8",
  });
  assert.strictEqual(serviceInstallHelp.status, 0, serviceInstallHelp.stderr);
  assert.match(serviceInstallHelp.stdout, /octo-daemon service control \(npm shim \/ pm2\)/);
  assert.doesNotMatch(serviceInstallHelp.stdout, /fake (darwin|linux)\/(amd64|arm64)/);
});

test("shim owns pm2 service start and writes ecosystem for Go run", (t) => {
  const key = `${process.platform}-${process.arch}`;
  const supported = new Set(PLATFORMS.map((p) => `${p.npmOs}-${p.npmCpu}`));
  if (!supported.has(key) || process.platform === "win32") {
    t.skip(`host ${key} not supported by this POSIX pm2 shim test`);
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-service-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const distDir = path.join(tmp, "dist");
  const outDir = path.join(tmp, "out");
  makeDist(distDir);
  assert.strictEqual(run(["--version", VERSION, "--dist", distDir, "--out", outDir]).status, 0);

  const pkgRoot = path.join(tmp, "node_modules", "@mininglamp-oss");
  fs.mkdirSync(pkgRoot, { recursive: true });
  fs.cpSync(path.join(outDir, `octo-daemon-${key}`), path.join(pkgRoot, `octo-daemon-${key}`), {
    recursive: true,
  });
  const mainDir = path.join(pkgRoot, "octo-daemon");
  fs.cpSync(path.join(outDir, "octo-daemon"), mainDir, { recursive: true });

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const home = path.join(tmp, "home");
  const cfgPath = path.join(tmp, "with space", "config.json");
  fs.mkdirSync(path.dirname(cfgPath), { recursive: true });
  fs.writeFileSync(cfgPath, "{}");

  const res = spawnSync(process.execPath, [
    path.join(mainDir, "bin", "octo-daemon.js"),
    "start",
    "--config",
    cfgPath,
  ], {
    encoding: "utf8",
    env: { ...process.env, HOME: home, PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 0, res.stderr);

  const ecoPath = path.join(home, ".octo-daemon", "ecosystem.config.js");
  const eco = fs.readFileSync(ecoPath, "utf8");
  assert.match(eco, /Generated by `octo-daemon start`/);
  assert.match(eco, /interpreter: "none"/);
  assert.match(eco, /args: \["run","--config",/);
  assert.ok(eco.includes(JSON.stringify(path.resolve(cfgPath))));

  const pm2Calls = fs.readFileSync(pm2Log, "utf8").trim().split("\n");
  assert.deepStrictEqual(pm2Calls, [`startOrRestart ${ecoPath}`, "save"]);
});

test("shim service stop is a pm2 operation", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stop-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "running", locked: true, pid: 4242, pid_file_stale: false }),
  );

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":4242,"pm2_env":{"status":"online"}}]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "stop"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.deepStrictEqual(fs.readFileSync(pm2Log, "utf8").trim().split("\n"), [
    "stop octo-daemon",
    "save",
  ]);
});

test("shim service stop continues when daemon status probe fails", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stop-status-fail-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(t, mainPackageDir, fakeFailingStatusBinary());

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":4242,"pm2_env":{"status":"online"}}]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "stop"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.match(res.stderr, /continuing stop without daemon lock status/);
  assert.match(res.stderr, /status unavailable/);
  assert.deepStrictEqual(fs.readFileSync(pm2Log, "utf8").trim().split("\n"), [
    "stop octo-daemon",
    "save",
  ]);
});

test("shim service stop treats pm2 transition states as managed", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stop-launching-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "running", locked: true, pid: 4242, pid_file_stale: false }),
  );

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":0,"pm2_env":{"status":"launching"}}]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "stop"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.doesNotMatch(res.stderr, /foreground mode/);
  assert.deepStrictEqual(fs.readFileSync(pm2Log, "utf8").trim().split("\n"), [
    "stop octo-daemon",
    "save",
  ]);
});

test("shim service stop continues when pm2 status probe fails", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stop-pm2-fail-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "stopped", locked: false, pid: 0, pid_file_stale: false }),
  );

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s\\n' 'pm2 jlist failed' >&2
  exit 7
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "stop"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.match(res.stderr, /continuing stop without pm2 service status/);
  assert.match(res.stderr, /pm2 jlist failed/);
  assert.doesNotMatch(res.stdout, /service is not installed/);
  assert.deepStrictEqual(fs.readFileSync(pm2Log, "utf8").trim().split("\n"), [
    "stop octo-daemon",
    "save",
  ]);
});

test("shim ignores stale pid files when the Go lock is free", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stale-pid-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "stopped", locked: false, pid: process.pid, pid_file_stale: true }),
  );

  const home = path.join(tmp, "home");
  const dataDir = path.join(home, ".octo-daemon");
  fs.mkdirSync(dataDir, { recursive: true });
  fs.writeFileSync(path.join(dataDir, "daemon.pid"), `${process.pid}\n`);

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "start"], {
    encoding: "utf8",
    env: {
      ...process.env,
      HOME: home,
      PATH: `${binDir}${path.delimiter}${process.env.PATH}`,
    },
  });
  assert.strictEqual(res.status, 0, res.stderr);
  assert.deepStrictEqual(fs.readFileSync(pm2Log, "utf8").trim().split("\n"), [
    `startOrRestart ${path.join(home, ".octo-daemon", "ecosystem.config.js")}`,
    "save",
  ]);
});

test("shim stop reports a foreground daemon even when a pm2 entry exists", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-stop-foreground-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "running", locked: true, pid: process.pid, pid_file_stale: false }),
  );

  const home = path.join(tmp, "home");

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":4242,"pm2_env":{"status":"stopped"}}]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "stop"], {
    encoding: "utf8",
    env: { ...process.env, HOME: home, PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 2, res.stderr);
  assert.match(res.stderr, /foreground mode/);
  assert.ok(!fs.existsSync(pm2Log), "pm2 stop should not run while a foreground daemon is active");
});

test("shim treats a held Go lock with unknown pid as foreground", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-lock-unknown-pid-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "running", locked: true, pid: 0, pid_file_stale: false }),
  );

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[]'
  exit 0
fi
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "start"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 2, res.stderr);
  assert.match(res.stderr, /foreground mode \(pid unknown\)/);
});

test("shim restart refuses to run while a foreground daemon is active", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-restart-foreground-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainPackageDir = path.join(tmp, "octo-daemon");
  const mainDir = path.join(mainPackageDir, "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));
  installFakePlatformBinary(
    t,
    mainPackageDir,
    fakeStatusBinary({ status: "running", locked: true, pid: process.pid, pid_file_stale: false }),
  );

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  const pm2Log = path.join(tmp, "pm2.log");
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":4242,"pm2_env":{"status":"online"}}]'
  exit 0
fi
printf '%s\\n' "$*" >> ${shellQuote(pm2Log)}
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "restart"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.strictEqual(res.status, 2, res.stderr);
  assert.match(res.stderr, /foreground mode/);
  assert.ok(!fs.existsSync(pm2Log), "pm2 restart should not run while a foreground daemon is active");
});

test("shim service subprocesses preserve signal exit semantics", (t) => {
  if (process.platform === "win32") {
    t.skip("fake pm2 shell script is POSIX-only");
    return;
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-signal-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const mainDir = path.join(tmp, "octo-daemon", "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));

  const binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  fs.writeFileSync(
    path.join(binDir, "pm2"),
    `#!/bin/sh
if [ "$1" = "jlist" ]; then
  printf '%s' '[{"name":"octo-daemon","pid":4242,"pm2_env":{"status":"online"}}]'
  exit 0
fi
if [ "$1" = "logs" ]; then
  kill -TERM $$
fi
exit 0
`,
    { mode: 0o755 },
  );

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "logs"], {
    encoding: "utf8",
    env: { ...process.env, HOME: path.join(tmp, "home"), PATH: `${binDir}${path.delimiter}${process.env.PATH}` },
  });
  assert.ok(res.signal === "SIGTERM" || res.status === 143, `status=${res.status} signal=${res.signal}`);
});

test("shim fails with a build-from-source pointer when the platform package is absent", (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-miss-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  // Shim copied alone — no node_modules next to it, so resolution fails.
  const mainDir = path.join(tmp, "octo-daemon", "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js"), "version"], {
    encoding: "utf8",
  });
  assert.strictEqual(res.status, 1);
  assert.match(res.stderr, /not installed|no prebuilt binary/);
});

test("fails loudly when an archive is missing", (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "prep-pkgs-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const distDir = path.join(tmp, "dist");
  makeDist(distDir);
  fs.rmSync(path.join(distDir, `octo-daemon_${VERSION}_linux_arm64.tar.gz`));

  const res = run(["--version", VERSION, "--dist", distDir, "--out", path.join(tmp, "out")]);
  assert.notStrictEqual(res.status, 0);
  assert.match(res.stderr, /missing release archive/);
});

test("rejects v-prefixed versions", () => {
  const res = run(["--version", "v1.0.0", "--dist", "/nonexistent", "--out", "/nonexistent2"]);
  assert.notStrictEqual(res.status, 0);
  assert.match(res.stderr, /bare semver/);
});

"use strict";

// Tests for prepare-packages.js: build a fake GoReleaser dist/ layout
// (tar.gz per posix platform, zip for windows), run the script, and assert
// the emitted package tree. Runs with `node --test` — no dependencies.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const { execFileSync, spawnSync } = require("child_process");

const SCRIPT = path.join(__dirname, "prepare-packages.js");
const VERSION = "9.9.9";

const PLATFORMS = [
  ["darwin", "arm64"],
  ["darwin", "amd64"],
  ["linux", "arm64"],
  ["linux", "amd64"],
  ["windows", "arm64"],
  ["windows", "amd64"],
];

function makeDist(distDir) {
  fs.mkdirSync(distDir, { recursive: true });
  for (const [goOs, goArch] of PLATFORMS) {
    const isZip = goOs === "windows";
    const binName = isZip ? "octo-daemon.exe" : "octo-daemon";
    const stage = fs.mkdtempSync(path.join(os.tmpdir(), "stage-"));
    fs.writeFileSync(path.join(stage, binName), `#!/bin/sh\necho fake ${goOs}/${goArch}\n`, {
      mode: 0o755,
    });
    const archive = path.join(
      distDir,
      `octo-daemon_${VERSION}_${goOs}_${goArch}.${isZip ? "zip" : "tar.gz"}`,
    );
    if (isZip) {
      execFileSync("zip", ["-j", "-q", archive, path.join(stage, binName)]);
    } else {
      execFileSync("tar", ["-czf", archive, "-C", stage, binName]);
    }
    fs.rmSync(stage, { recursive: true, force: true });
  }
}

function run(args) {
  return spawnSync(process.execPath, [SCRIPT, ...args], { encoding: "utf8" });
}

test("emits 6 platform packages and a pinned main package", (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "prep-pkgs-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  const distDir = path.join(tmp, "dist");
  const outDir = path.join(tmp, "out");
  makeDist(distDir);

  const res = run(["--version", VERSION, "--dist", distDir, "--out", outDir]);
  assert.strictEqual(res.status, 0, res.stderr);

  // Platform packages: os/cpu constraints + binary present and executable.
  const expectations = [
    ["darwin-arm64", "octo-daemon"],
    ["darwin-x64", "octo-daemon"],
    ["linux-arm64", "octo-daemon"],
    ["linux-x64", "octo-daemon"],
    ["win32-arm64", "octo-daemon.exe"],
    ["win32-x64", "octo-daemon.exe"],
  ];
  for (const [plat, binName] of expectations) {
    const pkgDir = path.join(outDir, `octo-daemon-${plat}`);
    const manifest = JSON.parse(fs.readFileSync(path.join(pkgDir, "package.json"), "utf8"));
    assert.strictEqual(manifest.name, `@mininglamp-oss/octo-daemon-${plat}`);
    assert.strictEqual(manifest.version, VERSION);
    const [npmOs, npmCpu] = plat.split("-");
    assert.deepStrictEqual(manifest.os, [npmOs]);
    assert.deepStrictEqual(manifest.cpu, [npmCpu]);
    const bin = path.join(pkgDir, "bin", binName);
    assert.ok(fs.existsSync(bin), `missing ${bin}`);
    assert.ok(fs.statSync(bin).mode & 0o100, `${bin} not executable`);
  }

  // Main package: version stamped, optionalDependencies exact-pinned to
  // all six platform packages, shim shipped, dev scripts stripped.
  const main = JSON.parse(
    fs.readFileSync(path.join(outDir, "octo-daemon", "package.json"), "utf8"),
  );
  assert.strictEqual(main.name, "@mininglamp-oss/octo-daemon");
  assert.strictEqual(main.version, VERSION);
  assert.strictEqual(Object.keys(main.optionalDependencies).length, 6);
  for (const [dep, pin] of Object.entries(main.optionalDependencies)) {
    assert.match(dep, /^@mininglamp-oss\/octo-daemon-(darwin|linux|win32)-(x64|arm64)$/);
    assert.strictEqual(pin, VERSION, `${dep} must be exact-pinned`);
  }
  assert.strictEqual(main.scripts, undefined);
  assert.ok(fs.existsSync(path.join(outDir, "octo-daemon", "bin", "octo-daemon.js")));

  // No stray extraction workdirs left behind.
  const leftovers = fs.readdirSync(outDir).filter((n) => n.startsWith(".extract-"));
  assert.deepStrictEqual(leftovers, []);
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

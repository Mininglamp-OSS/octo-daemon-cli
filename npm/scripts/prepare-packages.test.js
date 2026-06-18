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
    fs.writeFileSync(path.join(stage, "octo-daemon"), `#!/bin/sh\necho fake ${goOs}/${goArch} "$@"\n`, {
      mode: 0o755,
    });
    const archive = path.join(distDir, `octo-daemon_${VERSION}_${goOs}_${goArch}.tar.gz`);
    execFileSync("tar", ["-czf", archive, "-C", stage, "octo-daemon"]);
    fs.rmSync(stage, { recursive: true, force: true });
  }
}

function run(args) {
  return spawnSync(process.execPath, [SCRIPT, ...args], { encoding: "utf8" });
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
});

test("shim fails with a build-from-source pointer when the platform package is absent", (t) => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "shim-miss-"));
  t.after(() => fs.rmSync(tmp, { recursive: true, force: true }));
  // Shim copied alone — no node_modules next to it, so resolution fails.
  const mainDir = path.join(tmp, "octo-daemon", "bin");
  fs.mkdirSync(mainDir, { recursive: true });
  fs.copyFileSync(SHIM, path.join(mainDir, "octo-daemon.js"));

  const res = spawnSync(process.execPath, [path.join(mainDir, "octo-daemon.js")], {
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

#!/usr/bin/env node
"use strict";

// prepare-packages: repack GoReleaser release archives into npm packages.
//
//   node npm/scripts/prepare-packages.js --version 0.2.0 --dist <dir> --out <dir>
//
// Input  (--dist): the downloaded GitHub Release assets, i.e. GoReleaser
//   archives named `octo-daemon_<version>_<os>_<arch>.tar.gz` — the name
//   template lives in .goreleaser.yaml and must stay in lockstep with this
//   script (the test parses .goreleaser.yaml and asserts the matrices match).
// Output (--out):
//   <out>/octo-daemon-<npmOs>-<npmCpu>/   one package per platform, binary
//                                          inside `bin/`, os/cpu constrained
//   <out>/octo-daemon/                     the main package: shim + manifest
//                                          with optionalDependencies pinned
//                                          to the exact same version
//
// Publish order matters: sub-packages first, then the main package — an
// installer must never resolve a main package whose optionalDependencies
// are not yet on the registry. npm-publish.yml owns that sequencing; this
// script only lays out directories.
//
// Every failure exits non-zero with a message: a partial layout that gets
// published would ship a broken install, so loud failure is the only
// acceptable mode here.

const fs = require("fs");
const path = require("path");
const { execFileSync } = require("child_process");

const SCOPE = "@mininglamp-oss";
const PROJECT = "octo-daemon";

// goreleaser (os, arch) -> npm (os, cpu). Single source of truth for the
// platform matrix on the npm side; the guard test asserts set-equality with
// .goreleaser.yaml's goos × goarch. windows is absent on purpose — the Go
// code does not cross-compile for it yet (see .goreleaser.yaml).
const PLATFORMS = [
  { goOs: "darwin", goArch: "arm64", npmOs: "darwin", npmCpu: "arm64" },
  { goOs: "darwin", goArch: "amd64", npmOs: "darwin", npmCpu: "x64" },
  { goOs: "linux", goArch: "arm64", npmOs: "linux", npmCpu: "arm64" },
  { goOs: "linux", goArch: "amd64", npmOs: "linux", npmCpu: "x64" },
];

function fail(msg) {
  console.error(`[prepare-packages] ${msg}`);
  process.exit(1);
}

function parseArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const val = argv[i + 1];
    if (!key.startsWith("--") || val === undefined) fail(`bad argument: ${key}`);
    args[key.slice(2)] = val;
  }
  for (const required of ["version", "dist", "out"]) {
    if (!args[required]) fail(`--${required} is required`);
  }
  // Bare semver (no v prefix) — npm versions never carry the v.
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(args.version)) {
    fail(`--version must be bare semver, got '${args.version}'`);
  }
  return args;
}

function writeJson(file, obj) {
  fs.writeFileSync(file, JSON.stringify(obj, null, 2) + "\n");
}

function main() {
  const args = parseArgs(process.argv.slice(2));
  const version = args.version;
  const distDir = path.resolve(args.dist);
  const outDir = path.resolve(args.out);
  const npmDir = path.resolve(__dirname, "..");

  fs.rmSync(outDir, { recursive: true, force: true });
  fs.mkdirSync(outDir, { recursive: true });

  const optionalDependencies = {};

  for (const p of PLATFORMS) {
    const archive = path.join(distDir, `${PROJECT}_${version}_${p.goOs}_${p.goArch}.tar.gz`);
    if (!fs.existsSync(archive)) fail(`missing release archive: ${archive}`);

    const workDir = path.join(outDir, `.extract-${p.goOs}-${p.goArch}`);
    fs.mkdirSync(workDir, { recursive: true });
    execFileSync("tar", ["-xzf", archive, "-C", workDir]);
    const extracted = path.join(workDir, PROJECT);
    if (!fs.existsSync(extracted)) fail(`archive ${archive} does not contain ${PROJECT}`);

    const pkgName = `${SCOPE}/${PROJECT}-${p.npmOs}-${p.npmCpu}`;
    const pkgDir = path.join(outDir, `${PROJECT}-${p.npmOs}-${p.npmCpu}`);
    fs.mkdirSync(path.join(pkgDir, "bin"), { recursive: true });
    fs.copyFileSync(extracted, path.join(pkgDir, "bin", PROJECT));
    fs.chmodSync(path.join(pkgDir, "bin", PROJECT), 0o755);
    fs.rmSync(workDir, { recursive: true, force: true });

    writeJson(path.join(pkgDir, "package.json"), {
      name: pkgName,
      version,
      description: `${PROJECT} prebuilt binary for ${p.npmOs}-${p.npmCpu}.`,
      os: [p.npmOs],
      cpu: [p.npmCpu],
      files: [`bin/${PROJECT}`],
      engines: { node: ">=18" },
      homepage: "https://github.com/Mininglamp-OSS/octo-daemon-cli#readme",
      repository: {
        type: "git",
        url: "git+https://github.com/Mininglamp-OSS/octo-daemon-cli.git",
      },
      license: "Apache-2.0",
    });

    optionalDependencies[pkgName] = version; // exact pin, no range
    console.log(`[prepare-packages] ${pkgName}@${version} <- ${path.basename(archive)}`);
  }

  // Main package: copy the checked-in template + shim, stamp version and
  // the exact-pinned optionalDependencies.
  const mainDir = path.join(outDir, PROJECT);
  fs.mkdirSync(path.join(mainDir, "bin"), { recursive: true });
  fs.copyFileSync(
    path.join(npmDir, "bin", "octo-daemon.js"),
    path.join(mainDir, "bin", "octo-daemon.js"),
  );
  for (const doc of ["README.md", "LICENSE"]) {
    const src = path.join(npmDir, doc);
    const fallback = path.join(npmDir, "..", doc);
    fs.copyFileSync(fs.existsSync(src) ? src : fallback, path.join(mainDir, doc));
  }

  const manifest = JSON.parse(fs.readFileSync(path.join(npmDir, "package.json"), "utf8"));
  manifest.version = version;
  manifest.optionalDependencies = optionalDependencies;
  delete manifest.scripts; // dev-only (test runner); not part of the published package
  writeJson(path.join(mainDir, "package.json"), manifest);

  console.log(`[prepare-packages] ${SCOPE}/${PROJECT}@${version} (main, ${PLATFORMS.length} platform deps)`);
}

module.exports = { PLATFORMS };

if (require.main === module) {
  main();
}

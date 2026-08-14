"use strict";

const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawn } = require("node:child_process");
const {
  input,
  booleanFlag,
  mask,
  parseBoolean,
  request,
  resolveWriteMode,
  safeTemporaryDirectory,
  saveState,
  setOutput,
} = require("./lib");

const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function positiveInteger(name, fallback, maximum = Number.MAX_SAFE_INTEGER) {
  const value = Number.parseInt(input(name, String(fallback)), 10);
  if (!Number.isSafeInteger(value) || value <= 0 || value > maximum) {
    throw new Error(`${name} must be an integer between 1 and ${maximum}`);
  }
  return value;
}

async function main() {
  if (process.platform !== "linux") {
    throw new Error("this release supports Linux GitHub Actions runners only");
  }
  if (process.env.ACTIONS_CACHE_SERVICE_V2?.toLowerCase() !== "true") {
    throw new Error("GitHub Actions cache v2 is unavailable (ACTIONS_CACHE_SERVICE_V2 != true)");
  }
  if (!process.env.ACTIONS_RESULTS_URL || !process.env.ACTIONS_RUNTIME_TOKEN) {
    throw new Error("GitHub Actions cache v2 credentials are unavailable in this step");
  }

  const architecture = { x64: "amd64", arm64: "arm64" }[process.arch];
  if (!architecture) {
    throw new Error(`unsupported Linux architecture: ${process.arch}`);
  }
  const actionRoot = path.resolve(__dirname, "..");
  const binary = path.join(actionRoot, "dist", `cache-server-linux-${architecture}`);
  if (!fs.existsSync(binary)) {
    throw new Error(`packaged server binary is missing: ${binary}`);
  }

  const writeEnabled = resolveWriteMode(input("write", "auto"));
  const failOpen = !parseBoolean(input("fail-on-cache-error", "false"), "fail-on-cache-error");
  const maxBlobSizeMB = positiveInteger("max-blob-size-mb", 512, 10_240);
  const maxConcurrent = positiveInteger("max-concurrent-operations", 4, 32);
  const uploadsPerMinute = positiveInteger("max-uploads-per-minute", 180, 199);
  const backendTimeoutSeconds = positiveInteger("backend-timeout-seconds", 300, 3600);
  const storageMode = input("storage-mode", "objects").trim().toLowerCase();
  if (!["objects", "packs"].includes(storageMode)) {
    throw new Error("storage-mode must be objects or packs");
  }
  const packSizeMB = positiveInteger("pack-size-mb", 8, 32);
  const packFlushSeconds = positiveInteger("pack-flush-seconds", 30, 3600);
  const maxManifests = positiveInteger("max-manifests", 2048, 10_000);
  const githubToken = input("github-token", "");
  if (storageMode === "packs" && !githubToken) {
    throw new Error("storage-mode=packs requires github-token with actions: read");
  }
  if (githubToken) mask(githubToken);
  const portText = input("port", "0");
  const port = Number.parseInt(portText, 10);
  if (!Number.isSafeInteger(port) || port < 0 || port > 65535) {
    throw new Error("port must be an integer between 0 and 65535");
  }

  const tempDir = safeTemporaryDirectory(
    fs.mkdtempSync(path.join(path.resolve(process.env.RUNNER_TEMP), "bazel-gha-cache-v2-")),
  );
  const readyFile = path.join(tempDir, "ready.json");
  const statsFile = path.join(tempDir, "stats.json");
  const logFile = path.join(tempDir, "server.log");
  const spoolDir = path.join(tempDir, "spool");
  const shutdownToken = crypto.randomBytes(32).toString("hex");
  mask(shutdownToken);

  const args = [
    "--port",
    String(port),
    "--cache-dir",
    spoolDir,
    "--key-prefix",
    input("key-prefix", "bazel-http-v1"),
    "--storage-mode",
    storageMode,
    "--pack-size",
    String(packSizeMB * 1024 * 1024),
    "--pack-flush-interval",
    `${packFlushSeconds}s`,
    "--max-manifests",
    String(maxManifests),
    booleanFlag("write-enabled", writeEnabled),
    booleanFlag("fail-open", failOpen),
    "--max-blob-size",
    String(maxBlobSizeMB * 1024 * 1024),
    "--max-concurrent",
    String(maxConcurrent),
    "--uploads-per-minute",
    String(uploadsPerMinute),
    "--backend-timeout",
    `${backendTimeoutSeconds}s`,
    "--ready-file",
    readyFile,
    "--stats-file",
    statsFile,
  ];
  const logDescriptor = fs.openSync(logFile, "a", 0o600);
  const child = spawn(binary, args, {
    detached: true,
    env: {
      ...process.env,
      BAZEL_GHA_CACHE_SHUTDOWN_TOKEN: shutdownToken,
      GITHUB_TOKEN: githubToken || process.env.GITHUB_TOKEN || "",
    },
    stdio: ["ignore", logDescriptor, logDescriptor],
  });
  fs.closeSync(logDescriptor);
  child.unref();

  let ready;
  for (let attempt = 0; attempt < 150; attempt += 1) {
    if (fs.existsSync(readyFile)) {
      ready = JSON.parse(fs.readFileSync(readyFile, "utf8"));
      break;
    }
    if (child.exitCode !== null) break;
    await sleep(100);
  }
  if (!ready) {
    const log = fs.existsSync(logFile) ? fs.readFileSync(logFile, "utf8").slice(-8000) : "";
    throw new Error(`cache server did not become ready\n${log}`);
  }
  const health = await request(`${ready.url}/ready`, { method: "GET", timeout: 2000 });
  if (health.status !== 200) {
    throw new Error(`cache server readiness check returned HTTP ${health.status}`);
  }

  saveState("url", ready.url);
  saveState("pid", String(ready.pid));
  saveState("shutdown_token", shutdownToken);
  saveState("stats_file", statsFile);
  saveState("log_file", logFile);
  saveState("temp_dir", tempDir);
  saveState("shutdown_wait_seconds", String(Math.max(60, backendTimeoutSeconds + 30)));
  setOutput("url", ready.url);
  setOutput("stats-url", ready.stats_url);
  setOutput("writable", String(writeEnabled));
  setOutput(
    "bazel-args",
    `--remote_cache=${ready.url} --remote_upload_local_results=${writeEnabled}`,
  );
  setOutput("initial-stats", '{"requests":0,"hits":0,"misses":0,"uploads":0}');
  process.stdout.write(
    `Bazel cache adapter ready at ${ready.url} (write=${writeEnabled}, mode=${storageMode}, fail_open=${failOpen}, pid=${ready.pid})${os.EOL}`,
  );
}

main().catch((error) => {
  process.stderr.write(`::error::${String(error.message).replaceAll("\r", "").replaceAll("\n", "%0A")}\n`);
  process.exitCode = 1;
});

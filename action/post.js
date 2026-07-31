"use strict";

const fs = require("node:fs");
const { request, safeTemporaryDirectory, setOutput } = require("./lib");

const sleep = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function post() {
  const url = process.env.STATE_url;
  const token = process.env.STATE_shutdown_token;
  const pid = Number.parseInt(process.env.STATE_pid || "", 10);
  const statsFile = process.env.STATE_stats_file;
  const logFile = process.env.STATE_log_file;
  const tempDir = process.env.STATE_temp_dir;
  if (!url || !token || !Number.isSafeInteger(pid)) {
    process.stdout.write("Cache server state is absent; no cleanup is needed.\n");
    return;
  }

  try {
    const response = await request(`${url}/shutdown`, {
      method: "POST",
      headers: { "X-Shutdown-Token": token },
      timeout: 3000,
    });
    if (response.status !== 202) {
      process.stderr.write(`::warning::cache shutdown returned HTTP ${response.status}\n`);
    }
  } catch (error) {
    process.stderr.write(`::warning::cache shutdown request failed: ${error.message}\n`);
  }

  for (let attempt = 0; attempt < 100 && processExists(pid); attempt += 1) {
    await sleep(100);
  }
  if (processExists(pid)) {
    process.stderr.write("::warning::cache server did not stop gracefully; sending SIGTERM\n");
    try {
      process.kill(pid, "SIGTERM");
    } catch {}
  }

  let stats = {};
  if (statsFile && fs.existsSync(statsFile)) {
    try {
      stats = JSON.parse(fs.readFileSync(statsFile, "utf8"));
      setOutput("final-stats", JSON.stringify(stats));
    } catch (error) {
      process.stderr.write(`::warning::cannot read final cache statistics: ${error.message}\n`);
    }
  }
  if (process.env.GITHUB_STEP_SUMMARY) {
    const summary = [
      "### Bazel GitHub Actions cache v2",
      "",
      "| Metric | Value |",
      "|---|---:|",
      `| Hits | ${stats.hits ?? 0} |`,
      `| Misses | ${stats.misses ?? 0} |`,
      `| Published uploads | ${stats.uploads ?? 0} |`,
      `| Read-only discarded uploads | ${stats.discarded_uploads ?? 0} |`,
      `| Backend downloads | ${stats.backend_downloads ?? 0} |`,
      `| Backend existence checks | ${stats.backend_existence_checks ?? 0} |`,
      `| Backend load errors | ${stats.backend_load_errors ?? 0} |`,
      `| Backend save errors | ${stats.backend_save_errors ?? 0} |`,
      `| Validated action results | ${stats.validated_action_results ?? 0} |`,
      `| Incomplete action results | ${stats.incomplete_action_results ?? 0} |`,
      `| Invalid action results | ${stats.invalid_action_results ?? 0} |`,
      `| Skipped action-result uploads | ${stats.skipped_action_result_uploads ?? 0} |`,
      `| Bytes served | ${stats.bytes_served ?? 0} |`,
      `| Bytes received | ${stats.bytes_received ?? 0} |`,
      "",
    ].join("\n");
    fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, summary);
  }

  if (logFile && fs.existsSync(logFile)) {
    const log = fs.readFileSync(logFile, "utf8");
    process.stdout.write(`Cache server log:\n${log.slice(-16000)}`);
  }
  if (tempDir) {
    try {
      fs.rmSync(safeTemporaryDirectory(tempDir), { recursive: true, force: true });
    } catch (error) {
      process.stderr.write(`::warning::cannot remove cache temporary directory: ${error.message}\n`);
    }
  }
}

post().catch((error) => {
  process.stderr.write(`::warning::cache post-step failed: ${error.message}\n`);
});

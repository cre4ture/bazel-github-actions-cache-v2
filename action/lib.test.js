"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { appendCommand, isForkPullRequest, resolveWriteMode, safeTemporaryDirectory } = require("./lib");

test("fork pull requests are always read-only", () => {
  const payload = {
    pull_request: {
      head: { repo: { full_name: "fork/repo" } },
      base: { repo: { full_name: "owner/repo" } },
    },
  };
  assert.equal(isForkPullRequest(payload), true);
  assert.equal(resolveWriteMode("true", payload), false);
});

test("auto writes only on a default-branch push", () => {
  const previousEvent = process.env.GITHUB_EVENT_NAME;
  const previousRef = process.env.GITHUB_REF;
  process.env.GITHUB_EVENT_NAME = "push";
  process.env.GITHUB_REF = "refs/heads/main";
  assert.equal(resolveWriteMode("auto", { repository: { default_branch: "main" } }), true);
  process.env.GITHUB_REF = "refs/heads/feature";
  assert.equal(resolveWriteMode("auto", { repository: { default_branch: "main" } }), false);
  process.env.GITHUB_EVENT_NAME = previousEvent;
  process.env.GITHUB_REF = previousRef;
});

test("command writer uses multiline syntax", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "command-test-"));
  const file = path.join(directory, "output");
  appendCommand(file, "value", "a\nb");
  assert.match(fs.readFileSync(file, "utf8"), /^value<<gha_cache_/);
  fs.rmSync(directory, { recursive: true });
});

test("temporary directory must be below RUNNER_TEMP", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "runner-temp-"));
  const previous = process.env.RUNNER_TEMP;
  process.env.RUNNER_TEMP = directory;
  assert.equal(safeTemporaryDirectory(path.join(directory, "child")), path.join(directory, "child"));
  assert.throws(() => safeTemporaryDirectory(directory));
  assert.throws(() => safeTemporaryDirectory(path.dirname(directory)));
  process.env.RUNNER_TEMP = previous;
  fs.rmSync(directory, { recursive: true });
});

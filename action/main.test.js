"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");

const { startupTimeoutMilliseconds } = require("./main");

test("packed startup waits for its configured manifest-discovery deadline", () => {
  assert.equal(startupTimeoutMilliseconds("packs", 300), 300_000);
});

test("object startup retains the short local-server readiness deadline", () => {
  assert.equal(startupTimeoutMilliseconds("objects", 300), 15_000);
});

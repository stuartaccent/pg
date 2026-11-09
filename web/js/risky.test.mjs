import assert from "node:assert/strict";
import { test } from "node:test";
import { warningFor } from "./risky.mjs";

test("Run warns about a write to every row, and nothing else", () => {
	assert.match(warningFor(undefined, true), /no where clause/);
	assert.equal(warningFor(undefined, false), "");
});

test("Explain doesn't run anything, so doesn't warn", () => {
	assert.equal(warningFor("1", true), "");
});

test("Analyze always warns, as it runs the query", () => {
	assert.match(warningFor("analyze", false), /really runs/);
	assert.match(warningFor("analyze", true), /really runs/);
});

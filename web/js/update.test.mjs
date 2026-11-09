import assert from "node:assert/strict";
import { test } from "node:test";
import { updateSQL } from "./update.mjs";

const table = { name: "public.t", columns: { 1: "id", 2: "k2", 3: '"Name"', 4: "note" }, key: [1, 2] };
// select t.id, t.k2, t."Name", t.note, o.x from t join o …
const headers = [
	{ table: "10", attr: "1" },
	{ table: "10", attr: "2" },
	{ table: "10", attr: "3" },
	{ table: "10", attr: "4" },
	{ table: "20", attr: "1" },
	{},
];
const row = [
	{ text: "7", null: false },
	{ text: "b", null: false },
	{ text: "it's", null: false },
	{ text: "", null: true },
	{ text: "other", null: false },
	{ text: "1", null: false },
];

test("a cell sets only its column, keyed on the composite primary key", () => {
	assert.deepEqual(updateSQL(table, headers, row, 2, false), {
		sql: `update public.t\nset "Name" = 'it''s'\nwhere id = '7'\n  and k2 = 'b';\n`,
	});
});

test("a row sets every column of its table but the key", () => {
	assert.equal(updateSQL(table, headers, row, 2, true).sql, `update public.t\nset "Name" = 'it''s',\n    note = null\nwhere id = '7'\n  and k2 = 'b';\n`);
});

test("refuses without the whole key", () => {
	assert.match(updateSQL(table, headers.slice(1), row.slice(1), 1, false).error, /don't include/);
	assert.match(updateSQL({ ...table, key: [] }, headers, row, 2, false).error, /no primary key/);
	const outer = row.with(0, { text: "", null: true });
	assert.match(updateSQL(table, headers, outer, 2, false).error, /has no public.t row/);
});

test("refuses once the table has changed", () => {
	assert.match(updateSQL({ ...table, columns: { 1: "id", 2: "k2" } }, headers, row, 2, false).error, /has changed/);
});

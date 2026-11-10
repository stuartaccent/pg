import assert from "node:assert/strict";
import { test } from "node:test";
import { updateSQL } from "./update.mjs";

const table = { name: "public.t", columns: { 1: "id", 2: "k2", 3: '"Name"', 4: "note" }, key: [1, 2], generated: [], inherited: false };
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

test("refuses a table that appears twice, so the key matches what was clicked", () => {
	// select t.*, again.id, again.k2, again."Name" from t join t again …
	const twice = [...headers.slice(0, 4), { table: "10", attr: "1" }, { table: "10", attr: "2" }, { table: "10", attr: "3" }];
	const values = [...row.slice(0, 4), { text: "8", null: false }, { text: "c", null: false }, { text: "Bob", null: false }];
	assert.match(updateSQL(table, twice, values, 6, false).error, /more than once/);
	assert.match(updateSQL(table, twice, values, 2, false).error, /more than once/);
});

test("allows a column repeated with the same value, as in select id, *", () => {
	const repeated = [{ table: "10", attr: "1" }, ...headers];
	const values = [row[0], ...row];
	assert.equal(updateSQL(table, repeated, values, 3, false).sql, `update public.t\nset "Name" = 'it''s'\nwhere id = '7'\n  and k2 = 'b';\n`);
});

test("refuses a temporary table, which other tabs' sessions can't see", () => {
	assert.match(updateSQL({ ...table, temporary: true }, headers, row, 2, false).error, /temporary table/);
});

test("leaves generated columns out of a row, and refuses one as a cell", () => {
	const made = { ...table, generated: [4] };
	assert.equal(updateSQL(made, headers, row, 2, true).sql, `update public.t\nset "Name" = 'it''s'\nwhere id = '7'\n  and k2 = 'b';\n`);
	assert.match(updateSQL(made, headers, row, 3, false).error, /note is generated/);
});

test("refuses a table others inherit from", () => {
	assert.match(updateSQL({ ...table, inherited: true }, headers, row, 2, false).error, /inherit/);
});

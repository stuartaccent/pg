import assert from "node:assert/strict";
import { test } from "node:test";
import { tablesAt } from "./tables.mjs";

const names = (sql, pos = sql.length) => tablesAt(sql, pos).map(({ schema, table }) => (schema ? `${schema}.${table}` : table));

const cases = [
	["select * from t where ", ["t"]],
	["select * from public.t", ["public.t"]],
	["select * from T", ["t"]],
	['select * from "Odd ""name"', ['Odd "name']],
	['select * from "S"."T"', ["S.T"]],
	["select * from a, b", ["a", "b"]],
	["select * from a x, b as y where ", ["a", "b"]],
	["select * from a join b on a.id = b.id left join c using (id)", ["a", "b", "c"]],
	["select * from only a", ["a"]],
	["select * from a cross join lateral b", ["a", "b"]],
	["select * from a where id in (select id from b)", ["a", "b"]],
	["select * from (select * from a) x", ["a"]],
	["update a set ", ["a"]],
	["update only a set b = 1", ["a"]],
	["delete from a where ", ["a"]],
	["insert into a (", ["a"]],
	["with x as (select * from a) select * from x", ["a", "x"]],
	["select 'from a'", []],
	["select 1 -- from a", []],
	["select /* from a */ 1", []],
	["select $$ from a $$", []],
	["select 1", []],
	["select * from ", []],
];

for (const [sql, want] of cases) test(sql, () => assert.deepEqual(names(sql), want));

test("the cursor can be before the from", () => {
	assert.deepEqual(names("select  from a", 7), ["a"]);
});

test("only the statement under the cursor counts", () => {
	const sql = "select * from a; select * from b; select * from c";
	assert.deepEqual(names(sql, 5), ["a"]);
	assert.deepEqual(names(sql, 20), ["b"]);
	assert.deepEqual(names(sql, sql.length), ["c"]);
});

test("a semicolon in a string doesn't split the statement", () => {
	assert.deepEqual(names("select ';' from a"), ["a"]);
});

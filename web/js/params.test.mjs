import assert from "node:assert/strict";
import { test } from "node:test";
import { fillParams, paramNames } from "./params.mjs";

test("finds each name once, in order", () => {
	assert.deepEqual(paramNames("select :b, :a, :b from t where x = :_c1"), ["b", "a", "_c1"]);
	assert.deepEqual(paramNames("select array[:a, :b], x[:i]"), ["a", "b", "i"]);
	assert.deepEqual(paramNames("select (:a)::int,:b"), ["a", "b"]);
});

test("leaves everything else with a colon alone", () => {
	for (const sql of [
		"select a::int",
		"select a :: int",
		"select ':a'",
		'select ":a"',
		"select $$ :a $$",
		"select $x$ :a $x$",
		"-- :a\nselect 1",
		"/* :a */ select 1",
		"select a[1:2]",
		"select a[1:n]",
		"select a[1 : n]",
		"select f(a := 1)",
		"select : a",
		"select :1",
	]) {
		assert.deepEqual(paramNames(sql), [], sql);
	}
});

test("fills values in as literals", () => {
	assert.equal(fillParams("select :a, :a::int, ':a'", { a: "1" }), "select '1', '1'::int, ':a'");
	assert.equal(fillParams("select :a", { a: "it's" }), "select 'it''s'");
	assert.equal(fillParams("select :a; drop table t", { a: "'; drop table u; --" }), "select '''; drop table u; --'; drop table t");
	assert.equal(fillParams("select :a, :b", { a: "NULL", b: " null " }), "select null, null");
	assert.equal(fillParams("select :a", { a: "" }), "select ''");
});

test("leaves names with no value", () => {
	assert.equal(fillParams("select :a, :b", { b: "2" }), "select :a, '2'");
	assert.equal(fillParams("select :toString", {}), "select :toString");
});

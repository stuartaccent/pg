import assert from "node:assert/strict";
import { test } from "node:test";
import { writesEveryRow } from "./risky.mjs";

const risky = [
	"delete from t",
	"DELETE FROM t;",
	"update t set a = 1",
	"update only t set a = 1;",
	"select 1; delete from t",
	"-- a comment\ndelete from t",
	"/* where */ delete from t",
	"delete from t -- where id = 1",
	"update t set a = 'where'",
	'update t set "where" = 1',
	"update t set a = $$ where $$",
	"update t set a = $x$ where $x$",
	"update t set a = (select 1 from u where u.id = 1)",
	"with x as (select 1) delete from t",
	"with recursive x as (select 1 where true) update t set a = 1",
];

const safe = [
	"",
	"select 1",
	"select * from t for update",
	"select * from t for no key update",
	"delete from t where id = 1",
	"update t set a = 1 where id = 1",
	"UPDATE t SET a = 1 WHERE id = 1",
	"insert into t values (1) on conflict (id) do update set a = 1",
	"grant update on t to someone",
	"select 'delete from t'",
	"-- delete from t\nselect 1",
	"with x as (delete from t returning *) select * from x",
	"create function f() returns void language sql as $$ delete from t $$",
	"delete from t where id = 1; update u set a = 1 where id = 2",
];

for (const sql of risky) test(`risky: ${sql}`, () => assert.equal(writesEveryRow(sql), true));
for (const sql of safe) test(`safe: ${sql}`, () => assert.equal(writesEveryRow(sql), false));

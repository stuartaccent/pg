// Quoted, a value takes on whatever type its column is, so every type is
// written the same way.
const literal = (cell) => (cell.null ? "null" : `'${cell.text.replaceAll("'", "''")}'`);

// updateSQL writes an update for one row of a result, as { sql } or { error }.
// table is what /table returned for the clicked column's table. headers holds
// each result column's { table, attr }, empty if it came from no table; row
// each cell's { text, null }. With wholeRow it sets every column of that table
// in the result but the key, otherwise only the clicked one.
export function updateSQL(table, headers, row, column, wholeRow) {
	const oid = headers[column].table;
	const at = new Map(); // attnum to result column
	const same = (a, b) => a.null === b.null && a.text === b.text;
	for (const [i, h] of headers.entries()) {
		if (h.table !== oid) continue;
		// A column repeated with the same value, as in select id, *, is
		// harmless. Different values mean the table is joined to itself, and
		// the key could come from one copy and the value from another.
		if (at.has(h.attr) && !same(row[at.get(h.attr)], row[i])) return { error: `${table.name} appears more than once in the results` };
		if (!at.has(h.attr)) at.set(h.attr, i);
	}
	const clicked = headers[column].attr;
	if (table.temporary) return { error: `${table.name} is a temporary table, so only the tab that made it can change it` };
	if (table.inherited) return { error: `Other tables inherit from ${table.name}, and an update would change their rows too` };
	const key = table.key.map(String);
	const generated = table.generated.map(String);
	if (key.length === 0) return { error: `${table.name} has no primary key` };
	if (!key.every((k) => at.has(k))) return { error: `The results don't include ${table.name}'s primary key` };
	if (key.some((k) => row[at.get(k)].null)) return { error: `This row has no ${table.name} row` };

	let set = wholeRow ? [...at.keys()].filter((attr) => !key.includes(attr) && !generated.includes(attr)) : [clicked];
	if (set.length === 0) set = [clicked];
	const name = (attr) => table.columns[attr];
	if (![...set, ...key].every(name)) return { error: `${table.name} has changed: run the query again` };
	if (set.some((attr) => generated.includes(attr))) return { error: `${name(clicked)} is generated, so it can't be set` };

	const pairs = (attrs) => attrs.map((attr) => `${name(attr)} = ${literal(row[at.get(attr)])}`);
	return { sql: `update ${table.name}\nset ${pairs(set).join(",\n    ")}\nwhere ${pairs(key).join("\n  and ")};\n` };
}

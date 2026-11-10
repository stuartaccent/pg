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
	const copies = new Map(); // attnum to every result column showing it
	for (const [i, h] of headers.entries()) {
		if (h.table === oid) copies.set(h.attr, [...(copies.get(h.attr) ?? []), i]);
	}
	const at = new Map([...copies].map(([attr, columns]) => [attr, columns[0]]));
	const clicked = headers[column].attr;
	if (table.temporary) return { error: `${table.name} is a temporary table, so only the tab that made it can change it` };
	if (table.inherited) return { error: `Other tables inherit from ${table.name}, and an update would change their rows too` };
	const key = table.key.map(String);
	const generated = table.generated.map(String);
	if (key.length === 0) return { error: `${table.name} has no primary key` };
	// Columns shown more than once, as by select id, * or a join of the table
	// to itself, can't be told apart. Only when every copy has the whole key,
	// equal in each, are they all the same row, whichever was clicked.
	const most = Math.max(...[...copies.values()].map((columns) => columns.length));
	const same = (columns) => columns.every((i) => row[i].null === row[columns[0]].null && row[i].text === row[columns[0]].text);
	const oneRow = [...copies.values()].every(same) && key.every((k) => (copies.get(k)?.length ?? 0) === most);
	if (most > 1 && !oneRow) return { error: `${table.name} appears more than once in the results, so which row is meant isn't clear` };
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

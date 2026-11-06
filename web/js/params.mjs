import { sqlTokens } from "./tables.mjs";

const isName = (token) => /^[A-Za-z_]/.test(token ?? "");

// params finds each :name in sql, as { name, start, end }. Tokens skip strings
// and comments, which leaves the colons of ::casts, := and a[1:n] slices. Inside
// brackets, only one starting a subscript or a list item counts.
function params(sql) {
	const tokens = [...sqlTokens(sql)];
	const found = [];
	let brackets = 0;
	for (const [i, match] of tokens.entries()) {
		if (match[0] === "[") brackets++;
		else if (match[0] === "]") brackets--;
		if (match[0] !== ":") continue;
		const before = tokens[i - 1];
		const next = tokens[i + 1];
		if (!isName(next?.[0]) || next.index !== match.index + 1) continue;
		if (before?.[0] === ":" && before.index === match.index - 1) continue;
		if (brackets > 0 && !["[", ","].includes(before?.[0])) continue;
		found.push({ name: next[0], start: match.index, end: next.index + next[0].length });
	}
	return found;
}

// paramNames lists the different :names in sql, in order of first use.
export const paramNames = (sql) => [...new Set(params(sql).map((param) => param.name))];

// literal is value as SQL. Quoted, it takes on whatever type it's used as, so
// numbers and dates need no special handling.
const literal = (value) => (value.trim().toLowerCase() === "null" ? "null" : `'${value.replaceAll("'", "''")}'`);

// fillParams puts values, by name, in place of the :names in sql, leaving any
// it has no value for.
export function fillParams(sql, values) {
	let filled = "";
	let last = 0;
	for (const { name, start, end } of params(sql)) {
		if (!Object.hasOwn(values, name)) continue;
		filled += sql.slice(last, start) + literal(values[name]);
		last = end;
	}
	return filled + sql.slice(last);
}

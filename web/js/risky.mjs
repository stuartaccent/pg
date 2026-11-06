import { sqlTokens } from "./tables.mjs";

// statements splits sql into its top-level statements, with comments, quoted
// text and anything in parentheses taken out, so only real keywords remain.
function statements(sql) {
	const found = [];
	let words = [];
	let depth = 0;
	for (const [token] of sqlTokens(sql)) {
		if (/^(?:--|\/\*|['"$])/.test(token)) continue;
		if (token === "(") depth++;
		else if (token === ")") depth--;
		else if (depth > 0) continue;
		else if (token === ";") {
			found.push(words.join(" "));
			words = [];
		} else words.push(token);
	}
	found.push(words.join(" "));
	return found;
}

// writesEveryRow reports whether sql has an update or delete with no where.
// A with query's verb comes first at the top level, as its parts are in parentheses.
export function writesEveryRow(sql) {
	return statements(sql).some((statement) => {
		const verb = /^\s*(?:with\b.*?\s)?(select|insert|update|delete)\b/is.exec(statement)?.[1].toLowerCase();
		return (verb === "update" || verb === "delete") && !/\bwhere\b/i.test(statement);
	});
}

// warningFor is what to ask before sending sql, if anything. mode is the value
// of the button pressed: none for Run, "1" for Explain, "analyze" for Analyze.
export function warningFor(mode, sql) {
	if (mode === "analyze") {
		return "Analyze really runs this query to time it. Changes are rolled back, but it takes as long as the query, and anything outside the transaction, such as sequences, stays changed.";
	}
	if (!mode && writesEveryRow(sql)) {
		return "This has an update or delete with no where clause, so it changes every row.";
	}
	return "";
}

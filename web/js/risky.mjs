// statements splits sql into its top-level statements, with comments, quoted
// text and anything in parentheses taken out, so only real keywords remain.
function statements(sql) {
	const found = [];
	let current = "";
	let depth = 0;
	for (let i = 0; i < sql.length; i++) {
		const rest = sql.slice(i);
		const dollar = /^\$[A-Za-z_]*\$/.exec(rest);
		const skipTo = (end, from) => {
			const at = sql.indexOf(end, from);
			i = at < 0 ? sql.length : at + end.length - 1;
		};
		if (rest.startsWith("--")) skipTo("\n", i);
		else if (rest.startsWith("/*")) skipTo("*/", i + 2);
		else if (sql[i] === "'" || sql[i] === '"') skipTo(sql[i], i + 1);
		else if (dollar) skipTo(dollar[0], i + dollar[0].length);
		else if (sql[i] === "(") depth++;
		else if (sql[i] === ")") depth--;
		else if (depth > 0) continue;
		else if (sql[i] === ";") {
			found.push(current);
			current = "";
		} else {
			current += sql[i];
			continue;
		}
		current += " ";
	}
	found.push(current);
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

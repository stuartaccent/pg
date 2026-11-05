const tokenPattern = /--[^\n]*|\/\*[\s\S]*?\*\/|'(?:[^']|'')*'|\$(\w*)\$[\s\S]*?\$\1\$|"(?:[^"]|"")*"|[A-Za-z_][\w$]*|[^\s]/g;

// Words that can follow a table name, so aren't its alias.
const afterTable = new Set("where group having order union intersect except limit offset fetch for join inner left right full cross natural on using window set returning tablesample values".split(" "));

const isName = (token) => /^(?:[A-Za-z_"])/.test(token ?? "");

// Postgres folds unquoted names to lower case.
const unquote = (token) => (token[0] === '"' ? token.slice(1, -1).replaceAll('""', '"') : token.toLowerCase());

// tokensAt returns the tokens of the statement around pos, without comments.
function tokensAt(sql, pos) {
	let tokens = [];
	for (const match of sql.matchAll(tokenPattern)) {
		const token = match[0];
		if (token === ";") {
			if (match.index >= pos) break;
			tokens = [];
		} else if (!token.startsWith("--") && !token.startsWith("/*")) {
			tokens.push(token);
		}
	}
	return tokens;
}

// tablesAt lists the tables used by the statement around pos, as
// { schema, table }, with schema undefined if the name has none. It looks at
// the whole statement, as the cursor is often before the from. That includes
// any subqueries, as more columns on offer does no harm.
export function tablesAt(sql, pos) {
	const tokens = tokensAt(sql, pos);
	const tables = [];
	for (let i = 0; i < tokens.length; i++) {
		const word = tokens[i].toLowerCase();
		if (!["from", "join", "update", "into"].includes(word)) continue;
		i++;
		while (i < tokens.length) {
			while (["only", "lateral"].includes(tokens[i]?.toLowerCase())) i++;
			if (!isName(tokens[i])) break;
			const name = [unquote(tokens[i++])];
			while (tokens[i] === "." && isName(tokens[i + 1])) {
				name.push(unquote(tokens[i + 1]));
				i += 2;
			}
			tables.push({ schema: name.at(-2), table: name.at(-1) });
			if (tokens[i]?.toLowerCase() === "as") i++;
			if (isName(tokens[i]) && !afterTable.has(tokens[i].toLowerCase())) i++;
			if (tokens[i] !== ",") break;
			i++;
		}
		i--;
	}
	return tables;
}

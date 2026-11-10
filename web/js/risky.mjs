// warningFor is what to ask before sending a query, if anything. mode is the
// value of the button pressed: none for Run, "1" for Explain, "analyze" for
// Analyze. everyRow is whether the server found an update or delete with no where.
export function warningFor(mode, everyRow) {
	if (mode === "analyze") {
		return "Analyze really runs this query to time it. Changes are rolled back, but it takes as long as the query, and anything outside the transaction, such as sequences, stays changed.";
	}
	if (!mode && everyRow) {
		return "This has an update or delete with no where clause, or a merge on true, so it changes every row.";
	}
	return "";
}

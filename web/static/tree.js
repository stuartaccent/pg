// Puts a tree node's query in the editor. preventDefault stops the click
// also opening the node, as the name sits inside its <summary>.
function useSQL(event, el) {
	event.preventDefault();
	const sql = document.getElementById("sql");
	sql.value = el.dataset.sql;
	sql.focus();
}

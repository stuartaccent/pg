// The SQL editors, query tabs and tree actions. Bundled into web/static/app.js
// by `npm run build`.
import { basicSetup, EditorView } from "codemirror";
import { PostgreSQL, sql } from "@codemirror/lang-sql";
import { HighlightStyle, syntaxHighlighting, syntaxTree } from "@codemirror/language";
import { Compartment, Prec } from "@codemirror/state";
import { keymap, placeholder } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { formatDialect, postgresql } from "sql-formatter";
import { writesEveryRow } from "./risky.mjs";
import { tablesAt } from "./tables.mjs";

const views = new Map(); // tab id to its editor
const language = new Compartment();
let completions = {};
let activeTab;
const MAX_HISTORY = 2000;
const MAX_HISTORY_CHARS = 2_000_000; // localStorage holds about 5MB, and tabs' SQL must still fit
const MIN_PANE = 80; // the smallest a dragged pane gets; what's beside it keeps twice this, as it holds the buttons too

const sqlLanguage = () => sql({ dialect: PostgreSQL, schema: completions.schemas ?? {}, defaultSchema: completions.defaultSchema || undefined });

const quoteIfNeeded = (name) => (/^[a-z_][a-z0-9_$]*$/.test(name) ? name : `"${name.replaceAll('"', '""')}"`);

// Offers the columns of the tables in the statement under the cursor. lang-sql
// only completes them after a table name or alias and a dot.
function columnCompletions(context) {
	const word = context.matchBefore(/[\w$]*/);
	if (!word || (word.from === word.to && !context.explicit)) return null;
	if (context.state.sliceDoc(word.from - 1, word.from) === ".") return null;
	if (/String|Comment/.test(syntaxTree(context.state).resolveInner(context.pos, -1).name)) return null;

	const seen = new Set();
	const options = [];
	for (const { schema, table } of tablesAt(context.state.doc.toString(), context.pos)) {
		for (const column of completions.schemas?.[schema ?? completions.defaultSchema]?.[table] ?? []) {
			if (seen.has(column)) continue;
			seen.add(column);
			options.push({ label: column, apply: quoteIfNeeded(column), detail: table, type: "property", boost: 99 });
		}
	}
	return options.length ? { from: word.from, options, validFor: /^[\w$]*$/ } : null;
}

// Colours come from daisyUI's variables, so they follow the theme with no
// reconfiguring when it changes.
const theme = EditorView.theme({
	"&": { height: "100%", backgroundColor: "transparent", color: "var(--color-base-content)", fontSize: "13px" },
	"&.cm-focused": { outline: "none" },
	".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace", overflow: "auto" },
	".cm-content": { caretColor: "var(--color-base-content)" },
	".cm-cursor": { borderLeftColor: "var(--color-base-content)" },
	".cm-gutters": { backgroundColor: "transparent", border: "none", color: "color-mix(in oklab, var(--color-base-content) 40%, transparent)" },
	".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "color-mix(in oklab, var(--color-base-content) 5%, transparent)" },
	"&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection": {
		backgroundColor: "color-mix(in oklab, var(--color-primary) 30%, transparent)",
	},
	".cm-tooltip": { backgroundColor: "var(--color-base-200)", color: "var(--color-base-content)", border: "1px solid var(--color-base-300)" },
	".cm-tooltip-autocomplete > ul > li[aria-selected]": { backgroundColor: "var(--color-primary)", color: "var(--color-primary-content)" },
});

const highlight = HighlightStyle.define([
	{ tag: tags.keyword, color: "var(--color-primary)" },
	{ tag: [tags.string, tags.special(tags.string)], color: "var(--color-success)" },
	{ tag: [tags.number, tags.bool, tags.null], color: "var(--color-warning)" },
	{ tag: [tags.lineComment, tags.blockComment], color: "color-mix(in oklab, var(--color-base-content) 50%, transparent)", fontStyle: "italic" },
	{ tag: tags.typeName, color: "var(--color-accent)" },
]);

function mount(panel) {
	const textarea = panel.querySelector("textarea");
	const view = new EditorView({
		parent: textarea.form,
		extensions: [
			Prec.highest(keymap.of([
				{ key: "Mod-Enter", run: () => (textarea.form.requestSubmit(), true) },
				{ key: "Mod-Shift-f", run: () => (formatSQL(), true) },
			])),
			basicSetup,
			language.of(sqlLanguage()),
			PostgreSQL.language.data.of({ autocomplete: columnCompletions }),
			theme,
			syntaxHighlighting(highlight),
			placeholder("select 1;  (⌘/Ctrl+Enter to run)"),
			EditorView.updateListener.of((update) => {
				if (!update.docChanged) return;
				textarea.value = update.state.doc.toString();
				saveTabs();
			}),
		],
	});
	views.set(panel.dataset.tab, view);
	selectTab(panel.dataset.tab);
}

function selectTab(id) {
	activeTab = id;
	for (const panel of document.querySelectorAll("[data-tab]")) panel.hidden = panel.dataset.tab !== id;
	for (const tab of document.querySelectorAll("[data-tab-button]")) tab.classList.toggle("tab-active", tab.dataset.tabButton === id);
	views.get(id).focus();
}

async function closeTab(id) {
	if (views.size === 1) return;
	if (views.get(id).state.doc.length > 0 && !(await ask("Close this tab and discard its SQL?"))) return;
	await removeTab(id);
	if (activeTab === id) selectTab([...views.keys()].at(-1));
	saveTabs();
}

async function removeTab(id) {
	await fetch(`/tabs/${id}`, { method: "DELETE" });
	views.get(id).destroy();
	views.delete(id);
	document.querySelector(`[data-tab="${id}"]`).remove();
	document.querySelector(`[data-tab-button="${id}"]`).remove();
}

// There's always a tab, so the first one stays, emptied.
async function closeAllTabs() {
	const hasSQL = [...views.values()].some((view) => view.state.doc.length > 0);
	if (hasSQL && !(await ask("Close all tabs and discard their SQL?"))) return;
	const [kept, ...others] = views.keys();
	await Promise.all(others.map(removeTab));
	await fetch(`/tabs/${kept}`, { method: "DELETE" }); // ends its session, and any query running in it
	setText(views.get(kept), "");
	document.querySelector(`#results-${kept}`).replaceChildren();
	document.querySelector(`#notice-${kept}`).replaceChildren();
	selectTab(kept);
	saveTabs();
}

// ask shows the confirm dialog and resolves to whether the user continued.
// Not confirm(), which webviews don't have to implement.
function ask(message) {
	const dialog = document.querySelector("#confirm");
	document.querySelector("#confirm-text").textContent = message;
	dialog.returnValue = "";
	dialog.showModal();
	return new Promise((resolve) => {
		dialog.addEventListener("close", () => resolve(dialog.returnValue === "yes"), { once: true });
	});
}

const saveTabs = () => localStorage.setItem("tabs", JSON.stringify([...views.values()].map((view) => view.state.doc.toString())));

function setText(view, text) {
	view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
}

const newTab = async () => {
	await htmx.ajax("POST", "/tabs", { target: "#panels", swap: "beforeend" });
	mountNewPanels();
};

// Brings back the SQL from last time, one tab each. A page load always starts
// with a single empty tab from the server.
async function restoreTabs() {
	mountNewPanels();
	restoreSizes();
	const first = activeTab;
	const saved = JSON.parse(localStorage.getItem("tabs") ?? "[]").filter(Boolean);
	for (const [i, text] of saved.entries()) {
		if (i > 0) await newTab();
		setText(views.get(activeTab), text);
	}
	selectTab(first);
}

function mountNewPanels() {
	for (const panel of document.querySelectorAll("[data-tab]")) {
		if (!views.has(panel.dataset.tab)) mount(panel);
	}
}

// Shows text in the current tab if it's empty, otherwise in a new one, so
// browsing the tree never loses SQL.
async function openSQL(text) {
	if (views.get(activeTab).state.doc.length > 0) await newTab();
	const view = views.get(activeTab);
	setText(view, text);
	view.focus();
}

// sqlToRun is what a tab's buttons act on: the selection if there is one.
function sqlToRun(elt) {
	const { state } = views.get(elt.closest("[data-tab]").dataset.tab);
	const { from, to, empty } = state.selection.main;
	return empty ? state.doc.toString() : state.sliceDoc(from, to);
}

function formatSQL() {
	const view = views.get(activeTab);
	const notice = document.querySelector(`#notice-${activeTab}`);
	notice.textContent = "";
	try {
		setText(view, formatDialect(view.state.doc.toString(), { dialect: postgresql, keywordCase: "lower" }));
	} catch (error) {
		notice.textContent = `Could not format: ${error.message}`;
	}
}

const loadHistory = () => JSON.parse(localStorage.getItem("history") ?? "[]");

function remember(sql) {
	let history = [sql, ...loadHistory().filter((entry) => entry !== sql)].slice(0, MAX_HISTORY);
	while (history.length > 1 && JSON.stringify(history).length > MAX_HISTORY_CHARS) {
		history = history.slice(0, Math.ceil(history.length * 0.9));
	}
	localStorage.setItem("history", JSON.stringify(history));
}

function renderHistory() {
	const term = document.querySelector("#history-search").value.toLowerCase();
	const list = document.querySelector("#history-list");
	list.replaceChildren(...loadHistory().filter((sql) => sql.toLowerCase().includes(term)).map((sql) => {
		const item = document.createElement("li");
		const button = document.createElement("button");
		button.type = "button";
		button.className = "font-mono text-xs block truncate";
		button.textContent = button.title = sql;
		button.onclick = () => {
			document.querySelector("#history").close();
			openSQL(sql);
		};
		item.append(button);
		return item;
	}));
}

function showHistory() {
	document.querySelector("#history-search").value = "";
	renderHistory();
	document.querySelector("#history").showModal();
}

let openCell;

function showCell(td) {
	openCell = td;
	const text = td.hasAttribute("data-null") ? "NULL" : td.textContent;
	let shown = text;
	if (/^\s*[{[]/.test(text)) {
		try {
			shown = JSON.stringify(JSON.parse(text), null, 2);
		} catch {
			// Not JSON after all: show it as it is.
		}
	}
	document.querySelector("#cell-text").textContent = shown;
	document.querySelector("#cell").showModal();
}

const cellValue = (td) => (td.hasAttribute("data-null") ? "" : td.textContent);

async function copyCell(button, wholeRow) {
	const text = wholeRow ? [...openCell.parentElement.cells].map(cellValue).join("\t") : cellValue(openCell);
	let copied = true;
	try {
		await navigator.clipboard.writeText(text);
	} catch {
		// The clipboard API needs a secure context, which a webview's scheme may not count as.
		const scratch = document.createElement("textarea");
		scratch.value = text;
		button.closest("dialog").append(scratch);
		scratch.select();
		copied = document.execCommand("copy");
		scratch.remove();
	}
	const label = button.textContent;
	button.textContent = copied ? "Copied" : "Copy failed";
	setTimeout(() => (button.textContent = label), 1000);
}

// Tree buttons sit inside a <summary>: preventDefault stops them also toggling it.
function useSQL(event, el) {
	event.preventDefault();
	openSQL(el.dataset.sql);
}

async function useDefinition(event, el) {
	event.preventDefault();
	const res = await fetch(el.dataset.url);
	const text = await res.text();
	openSQL(res.ok ? text : `-- ${text.trim().replaceAll("\n", "\n-- ")}`);
}

// Reloads what the editors complete from, after a connect or refresh.
async function loadSchema() {
	completions = await (await fetch("/completions")).json();
	for (const view of views.values()) view.dispatch({ effects: language.reconfigure(sqlLanguage()) });
}

function resetTree() {
	document.querySelector("#tree-search").value = "";
	loadSchema();
}

// Drag bars, rather than CSS resize: WebKit's corner handle can't be relied on
// with an editor filling the box. Each sizes the element just before it, by a
// variable so that all the tabs' editors stay the same height.
const sizeVars = { height: "--editor-height", width: "--sidebar-width" };

function resize(bar, size) {
	const box = bar.previousElementSibling;
	const prop = bar.dataset.resize;
	const room = prop === "height" ? box.parentElement.clientHeight : box.parentElement.clientWidth;
	const px = Math.max(MIN_PANE, Math.min(size, room - 2 * MIN_PANE));
	document.documentElement.style.setProperty(sizeVars[prop], `${px}px`);
	localStorage.setItem(sizeVars[prop], px);
}

function restoreSizes() {
	for (const bar of document.querySelectorAll("[data-resize]")) {
		const saved = Number(localStorage.getItem(sizeVars[bar.dataset.resize]));
		if (saved) resize(bar, saved);
	}
}

function sizeOf(bar) {
	return bar.previousElementSibling.getBoundingClientRect()[bar.dataset.resize];
}

document.addEventListener("pointerdown", (event) => {
	const bar = event.target.closest("[data-resize]");
	if (!bar) return;
	event.preventDefault();
	bar.focus();
	const axis = bar.dataset.resize === "height" ? "clientY" : "clientX";
	const start = event[axis];
	const startSize = sizeOf(bar);
	const move = (e) => resize(bar, startSize + e[axis] - start);
	const stop = () => {
		window.removeEventListener("pointermove", move);
		window.removeEventListener("pointerup", stop);
		window.removeEventListener("pointercancel", stop);
	};
	window.addEventListener("pointermove", move);
	window.addEventListener("pointerup", stop);
	window.addEventListener("pointercancel", stop);
});

document.addEventListener("keydown", (event) => {
	const bar = event.target.closest?.("[data-resize]");
	const step = { ArrowDown: 20, ArrowRight: 20, ArrowUp: -20, ArrowLeft: -20 }[event.key];
	if (!bar || !step) return;
	event.preventDefault();
	resize(bar, sizeOf(bar) + step);
});

Object.assign(window, { selectTab, closeTab, closeAllTabs, useSQL, useDefinition, resetTree, formatSQL, showHistory, renderHistory, copyCell });

document.addEventListener("click", (event) => {
	const td = event.target.closest?.(".results tr[data-row] td");
	if (td && getSelection().isCollapsed) showCell(td);
});

// Sends the selection rather than the whole editor, and remembers what ran.
document.addEventListener("htmx:configRequest", (event) => {
	const { elt, formData, path } = event.detail;
	if (!formData.has("sql")) return;
	const sql = sqlToRun(elt);
	formData.set("sql", sql);
	if (path === "/query" && sql.trim()) remember(sql.trim());
});

// Counts up beside Running…, to tell a slow query from a stuck one.
document.addEventListener("htmx:beforeSend", (event) => {
	const { elt } = event.detail;
	const timer = elt.closest?.("[data-tab]")?.querySelector(".timer");
	if (!timer || !event.detail.requestConfig.formData?.has("sql")) return;
	clearInterval(timer.interval);
	const start = performance.now();
	timer.textContent = "";
	timer.interval = setInterval(() => (timer.textContent = `${((performance.now() - start) / 1000).toFixed(1)}s`), 100);
});

document.addEventListener("htmx:afterRequest", (event) => {
	const timer = event.detail.elt.closest?.("[data-tab]")?.querySelector(".timer");
	if (timer) clearInterval(timer.interval);
});

// Runs before htmx sends a query, for the ones worth a second thought.
document.addEventListener("htmx:confirm", (event) => {
	const { elt, triggeringEvent } = event.detail;
	if (!elt.matches("form[hx-post='/query']")) return;
	const mode = triggeringEvent?.submitter?.value;
	const sql = sqlToRun(elt);
	let warning;
	if (mode === "analyze") {
		warning = "Analyze really runs this query to time it. Changes are rolled back, but it takes as long as the query, and anything outside the transaction, such as sequences, stays changed.";
	} else if (!mode && writesEveryRow(sql)) {
		warning = "This has an update or delete with no where clause, so it changes every row.";
	}
	if (!warning) return;
	event.preventDefault();
	ask(warning).then((ok) => ok && event.detail.issueRequest(true));
});

htmx.onLoad(mountNewPanels);
document.addEventListener("DOMContentLoaded", () => {
	restoreTabs();
	loadSchema();
	reconnect();
});
document.addEventListener("connected", () => {
	localStorage.setItem("service", document.querySelector("select[name=service]").value);
	resetTree();
});

// Picks up the last service, unless it has gone from the service file.
function reconnect() {
	const select = document.querySelector("select[name=service]");
	const saved = localStorage.getItem("service");
	if (select.value || ![...select.options].some((option) => option.value === saved)) return;
	select.value = saved;
	select.dispatchEvent(new Event("change", { bubbles: true }));
}

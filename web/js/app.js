// The SQL editors, query tabs and tree actions. Bundled into web/static/app.js
// by `npm run build`.
import { basicSetup, EditorView } from "codemirror";
import { PostgreSQL, sql } from "@codemirror/lang-sql";
import { HighlightStyle, syntaxHighlighting, syntaxTree } from "@codemirror/language";
import { Compartment, Prec } from "@codemirror/state";
import { keymap, placeholder } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { formatDialect, postgresql } from "sql-formatter";
import {
	Activity, AlignLeft, Archive, Bookmark, Box, ChevronDown, Columns3, Copy, CopyX, Database, Download, Eye, FileCode, FileInput,
	FilePen, Folder, FolderOpen, Gauge, Hash, HeartPulse, History, KeyRound, Languages, Layers, ListTree, Play, Plus, RefreshCw, Save,
	Search, Server, Settings, Shapes, ShieldCheck, Square, SquareFunction, SquareTerminal, Star, SunMoon, Table2, Trash2, User, Users, X, Zap, createIcons,
} from "lucide";
import { fillParams, paramNames } from "./params.mjs";
import { warningFor } from "./risky.mjs";
import { tablesAt } from "./tables.mjs";
import { updateSQL } from "./update.mjs";

const views = new Map(); // tab id to its editor
const language = new Compartment();
let completions = {};
let activeTab;
const MIN_PANE = 80; // the smallest a dragged pane gets; what's beside it keeps twice this, as it holds the buttons too

// The icons the page uses, bundled so they work offline. The markup asks for
// one with <i data-lucide="name">; createIcons swaps it for the SVG.
const icons = {
	Activity, AlignLeft, Archive, Bookmark, Box, ChevronDown, Columns3, Copy, CopyX, Database, Download, Eye, FileCode, FileInput,
	FilePen, Folder, FolderOpen, Gauge, Hash, HeartPulse, History, KeyRound, Languages, Layers, ListTree, Play, Plus, RefreshCw, Save,
	Search, Server, Settings, Shapes, ShieldCheck, Square, SquareFunction, SquareTerminal, Star, SunMoon, Table2, Trash2, User, Users, X, Zap,
};
// Only within root: the SVGs keep data-lucide, so drawing the whole page
// again redraws every icon there, which made each tree expand slower.
// An icon loaded on its own is drawn from its parent, as root's own match isn't searched.
const drawIcons = (root) =>
	createIcons({ icons, root: root.matches?.("[data-lucide]") ? root.parentElement : root, attrs: { "aria-hidden": "true" } });

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
	// Restored tabs open behind, so whatever was chosen meanwhile stays in front.
	if (restoring) panel.hidden = true;
	else selectTab(panel.dataset.tab);
}

// The server tools, which open as tabs of their own beside the query tabs.
const tools = ["activity", "health", "backups", "history"];

// Selects a query tab or a tool. activeTab stays a query tab, as the
// editor's commands act on it.
function selectTab(id) {
	const tool = tools.includes(id);
	if (!tool) activeTab = id;
	for (const panel of document.querySelectorAll("[data-tool-panel]")) panel.hidden = panel.id !== `${id}-panel`;
	for (const panel of document.querySelectorAll("[data-tab]")) panel.hidden = tool || panel.dataset.tab !== id;
	for (const button of document.querySelectorAll("[data-tool-button]")) button.setAttribute("aria-pressed", button.dataset.toolButton === id);
	for (const tab of document.querySelectorAll("[data-tab-button]")) {
		const active = tab.dataset.tabButton === id;
		tab.classList.toggle("tab-active", active);
		tab.querySelector("[role=tab]")?.setAttribute("aria-selected", active);
	}
	if (!tool) views.get(id).focus();
}

// The tool showing, if any, and whether a tool's tab is open at all.
const toolShowing = () => tools.find((tool) => !document.querySelector(`#${tool}-panel`).hidden);
const toolOpen = (tool) => !document.querySelector(`#${tool}-tab`).hidden;

function openTool(tool) {
	document.querySelector(`#${tool}-tab`).hidden = false;
	selectTab(tool);
}

async function closeTab(id) {
	if (tools.includes(id)) {
		document.querySelector(`#${id}-tab`).hidden = true;
		if (toolShowing() === id) selectTab(activeTab);
		return;
	}
	if (views.size === 1) return;
	if (views.get(id).state.doc.length > 0 && !(await ask("Close this tab and discard its SQL?", true))) return;
	await removeTab(id);
	if (activeTab === id) {
		activeTab = [...views.keys()].at(-1);
		if (!toolShowing()) selectTab(activeTab);
	}
	saveTabs();
}

const closeActiveTab = () => closeTab(toolShowing() ?? activeTab);

// Runs a menu command, unless a dialog is open and it would act behind that.
function fromMenu(action) {
	if (!document.querySelector("dialog[open]")) action();
}

async function removeTab(id) {
	await fetch(`/tabs/${id}`, { method: "DELETE" });
	views.get(id).destroy();
	views.delete(id);
	tabQueries.delete(id);
	document.querySelector(`[data-tab="${id}"]`).remove();
	document.querySelector(`[data-tab-button="${id}"]`).remove();
}

// There's always a tab, so the first one stays, emptied.
async function closeAllTabs() {
	const hasSQL = [...views.values()].some((view) => view.state.doc.length > 0);
	if (hasSQL && !(await ask("Close all tabs and discard their SQL?", true))) return;
	const [kept, ...others] = views.keys();
	await Promise.all(others.map(removeTab));
	await fetch(`/tabs/${kept}`, { method: "DELETE" }); // ends its session, and any query running in it
	setText(views.get(kept), "");
	tabQueries.delete(kept);
	document.querySelector(`#results-${kept}`).replaceChildren();
	document.querySelector("#notice").replaceChildren();
	selectTab(kept);
	saveTabs();
}

// ask shows the confirm dialog and resolves to whether the user continued.
// Not confirm(), which webviews don't have to implement. Enter means Cancel
// unless enterContinues: a held ⌘+Enter mustn't wave through a risky query.
function ask(message, enterContinues = false) {
	const dialog = document.querySelector("#confirm");
	document.querySelector("#confirm-text").textContent = message;
	dialog.returnValue = "";
	dialog.showModal();
	if (enterContinues) dialog.querySelector("button[value=yes]").focus();
	return new Promise((resolve) => {
		dialog.addEventListener("close", () => resolve(dialog.returnValue === "yes"), { once: true });
	});
}

// askParams asks for the values of names, offering the last ones used, and
// resolves to them by name, or to nothing if cancelled.
function askParams(names) {
	const saved = JSON.parse(localStorage.getItem("params") ?? "{}");
	const dialog = document.querySelector("#params");
	const inputs = names.map((name) => {
		const input = document.createElement("input");
		input.className = "input input-sm w-full focus:outline-1 focus:outline-offset-0";
		input.id = `param-${name}`;
		input.autocomplete = "off";
		input.spellcheck = false;
		input.value = saved[name] ?? "";
		return input;
	});
	const labels = names.map((name, i) => {
		const label = document.createElement("label");
		label.className = "block text-xs text-base-content/50 mt-3 mb-1 font-mono";
		label.htmlFor = inputs[i].id;
		label.textContent = `:${name}`;
		return [label, inputs[i]];
	});
	document.querySelector("#params-fields").replaceChildren(...labels.flat());
	dialog.returnValue = "";
	dialog.showModal();
	inputs[0].select();
	return new Promise((resolve) => {
		dialog.addEventListener(
			"close",
			() => {
				if (dialog.returnValue !== "yes") return resolve();
				const values = Object.fromEntries(names.map((name, i) => [name, inputs[i].value]));
				localStorage.setItem("params", JSON.stringify({ ...saved, ...values }));
				resolve(values);
			},
			{ once: true },
		);
	});
}

const saveTabs = () => localStorage.setItem("tabs", JSON.stringify([...views.values()].map((view) => view.state.doc.toString())));

function setText(view, text) {
	view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
}

// Each htmx.ajax names its own source: without one they all share <body>, and
// htmx then queues a second request but resolves its promise at once, so
// awaiting newTab while a tool loaded went on before the tab existed.
const newTab = async () => {
	await htmx.ajax("POST", "/tabs", { source: "#panels", target: "#panels", swap: "beforeend" });
	mountNewPanels();
};

// Brings back the SQL from last time, one tab each. A page load always starts
// with a single empty tab from the server.
let restoring = false;

async function restoreTabs() {
	mountNewPanels();
	restoreSizes();
	const saved = JSON.parse(localStorage.getItem("tabs") ?? "[]").filter(Boolean);
	restoring = true;
	try {
		for (const [i, text] of saved.entries()) {
			if (i > 0) await newTab();
			setText([...views.values()].at(-1), text);
		}
	} finally {
		restoring = false;
	}
	// The first tab, unless another was clicked meanwhile, or a tool opened.
	if (!toolShowing()) selectTab(activeTab);
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
	setText(views.get(activeTab), text);
	selectTab(activeTab); // from behind a tool, if one is showing
}

// sqlToRun is what a tab's buttons act on: the selection if there is one.
function sqlToRun(elt) {
	const { state } = views.get(elt.closest("[data-tab]").dataset.tab);
	const { from, to, empty } = state.selection.main;
	return empty ? state.doc.toString() : state.sliceDoc(from, to);
}

function formatSQL() {
	const view = views.get(activeTab);
	const notice = document.querySelector("#notice");
	notice.textContent = "";
	try {
		setText(view, formatDialect(view.state.doc.toString(), { dialect: postgresql, keywordCase: "lower", paramTypes: { named: [":"] } }));
	} catch (error) {
		notice.textContent = `Could not format: ${error.message}`;
	}
}

// History moved to the server. What's left from before is just taking up room.
localStorage.removeItem("history");

// Opens a dialog with nothing focused. Left alone, the browser focuses its first
// link or button, and draws a focus ring round it that looks like a selection.
function showUnfocused(dialog) {
	dialog.showModal();
	document.activeElement.blur();
}

async function showBackups() {
	openTool("backups");
	await htmx.ajax("GET", "/backups", { source: "#backups-body", target: "#backups-body" });
}

// Shows a view in the sidebar: "tree", or "queries", which reloads its list
// each time, as files can change in Finder meanwhile.
function showSidebar(name) {
	for (const view of document.querySelectorAll("[data-sidebar]")) view.hidden = view.dataset.sidebar !== name;
	for (const button of document.querySelectorAll("[data-sidebar-button]")) button.setAttribute("aria-pressed", button.dataset.sidebarButton === name);
	localStorage.setItem("sidebar", name);
	if (name === "queries") return htmx.ajax("GET", "/queries", { source: "#queries-body", target: "#queries-body" });
}

const showQueries = () => showSidebar("queries");

// Which saved query each tab holds, to offer its name when saving again.
const tabQueries = new Map();

async function useQuery(event, el) {
	await useDefinition(event, el);
	tabQueries.set(activeTab, el.dataset.name);
}

// Saves with the app's Save dialog, or the page's own in a browser.
async function saveQuery() {
	selectTab(activeTab); // ⌘S over a tool saves the query tab, so show it
	const tab = activeTab;
	const name = tabQueries.get(tab) ?? "";
	const text = views.get(tab).state.doc.toString();
	const res = await fetch("/queries/choose", { method: "POST", body: new URLSearchParams({ name, text }) });
	if (res.status === 501) {
		showSaveQuery(name, text);
		return;
	}
	if (res.status === 204) return;
	const answer = (await res.text()).trim();
	document.querySelector("#notice").textContent = res.ok ? `Saved ${answer}` : answer;
	if (res.ok) {
		// Somewhere outside the folder isn't a name it can be offered under.
		if (answer.startsWith("/")) tabQueries.delete(tab);
		else tabQueries.set(tab, answer);
		if (localStorage.getItem("sidebar") === "queries") showSidebar("queries");
	}
}

// Opens a .sql file from anywhere with the app's Open dialog.
async function openQueryFile() {
	const notice = document.querySelector("#notice");
	const res = await fetch("/queries/pick", { method: "POST" });
	if (res.status === 204) return;
	if (!res.ok) {
		notice.textContent = res.status === 501 ? "Opening a file needs the app: use Saved queries here." : (await res.text()).trim();
		return;
	}
	const { name, text } = await res.json();
	await openSQL(text);
	if (!name.startsWith("/")) tabQueries.set(activeTab, name);
}

function showSaveQuery(name, text) {
	document.querySelector("#save-name").value = name;
	document.querySelector("#save-text").value = text;
	document.querySelector("#save-status").replaceChildren();
	document.querySelector("#save-query").showModal();
}

document.addEventListener("query-saved", () => {
	document.querySelector("#save-query").close();
	if (localStorage.getItem("sidebar") === "queries") showSidebar("queries");
	const name = document.querySelector("#save-name").value.trim().replace(/\.sql$/, "");
	tabQueries.set(activeTab, name);
	document.querySelector("#notice").textContent = `Saved ${name}`;
});

// Each tool opens its tab, or goes to it, and refreshes it now.
async function showActivity() {
	openTool("activity");
	await htmx.ajax("GET", "/activity", { source: "#activity-body", target: "#activity-body" });
}

// Opens at once with a spinner, as the checks can take a moment on a big database.
async function showHealth() {
	const body = document.querySelector("#health-body");
	body.innerHTML = '<span class="loading loading-spinner loading-sm"></span>';
	openTool("health");
	await htmx.ajax("GET", "/health", { source: body, target: body });
}

// Whether the list can refresh: only while showing. Not under a dialog:
// replacing the row behind a confirm would lose its Terminate. Nor while
// text is being selected.
const activityLive = () => toolShowing() === "activity" && !document.querySelector("dialog[open]") && getSelection().isCollapsed;

function showRestore(button) {
	const { file, name, service } = button.dataset;
	document.querySelector("#restore-backup").textContent = file;
	document.querySelector("#restore-service").textContent = service;
	document.querySelector("#restore-file").value = file;
	document.querySelector("#restore-name").value = name;
	document.querySelector("#restore").showModal();
}

function showReplace(button) {
	const { file, database, other } = button.dataset;
	document.querySelector("#replace-database").textContent = database;
	document.querySelector("#replace-file").textContent = file;
	document.querySelector("#replace-file-value").value = file;
	document.querySelector("#replace-other").hidden = other !== "true";
	document.querySelector("#replace-confirm").value = "";
	checkReplace();
	document.querySelector("#replace").showModal();
}

function checkReplace() {
	const typed = document.querySelector("#replace-confirm").value;
	document.querySelector("#replace-submit").disabled = typed !== document.querySelector("#replace-database").textContent;
}

const closeReplace = () => document.querySelector("#replace").close();
const closeRestore = () => document.querySelector("#restore").close();

async function showHistory() {
	const body = document.querySelector("#history-body");
	body.innerHTML = '<span class="loading loading-spinner loading-sm m-3"></span>';
	openTool("history");
	await htmx.ajax("GET", "/history", { source: body, target: body });
	document.querySelector("#history-search")?.focus();
}

// chooseHistory marks the History entry whose detail is showing. The form
// keeps its id, so the list still marks it after reloading.
function chooseHistory(button) {
	button.form.selected.value = button.dataset.id;
	for (const row of button.closest("tbody").querySelectorAll("tr[aria-current]")) row.removeAttribute("aria-current");
	button.closest("tr").setAttribute("aria-current", "true");
}

// sortHistory sorts the History list by a column's key, "" being last run.
function sortHistory(button, by) {
	const form = button.closest("form");
	form.sort.value = by;
	htmx.trigger(form, "input");
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
	// Only a column straight from a table can be updated.
	const fromTable = Boolean(headerOf(td).dataset.table);
	for (const button of document.querySelectorAll("#cell [data-update]")) button.hidden = !fromTable;
	showUnfocused(document.querySelector("#cell"));
}

const headerOf = (td) => td.closest("table").tHead.rows[0].cells[td.cellIndex];

// updateCell opens an update for the open cell, or its row, in a new tab.
async function updateCell(wholeRow) {
	const td = openCell;
	const res = await fetch(`/table?oid=${encodeURIComponent(headerOf(td).dataset.table)}`);
	const headers = [...td.closest("table").tHead.rows[0].cells].map((th) => ({ table: th.dataset.table, attr: th.dataset.attr }));
	const row = [...td.parentElement.cells].map((cell) => ({ text: cell.textContent, null: cell.hasAttribute("data-null") }));
	const { sql, error } = res.ok ? updateSQL(await res.json(), headers, row, td.cellIndex, wholeRow) : { error: (await res.text()).trim() };
	// Closed either way, so the status bar's message isn't hidden behind it.
	document.querySelector("#cell").close();
	if (sql) await openSQL(sql);
	document.querySelector("#notice").textContent = error ?? "Edit the values, then Run";
}

const cellValue = (td) => (td.hasAttribute("data-null") ? "" : td.textContent);

// rowJSON is a row as an object keyed by column name, NULL as null. Columns
// with no name (Activity's buttons) are left out; a repeated name gets " 2".
function rowJSON(tr) {
	const names = [...tr.closest("table").querySelectorAll("thead th")].map((th) => th.textContent.trim());
	const row = {};
	for (const [i, td] of [...tr.cells].entries()) {
		let name = names[i];
		if (!name) continue;
		for (let n = 2; Object.hasOwn(row, name); n++) name = `${names[i]} ${n}`;
		row[name] = td.hasAttribute("data-null") ? null : td.textContent;
	}
	return JSON.stringify(row, null, 2);
}

// Closes once copied, saying so in the status bar; stays open if it failed.
async function copyCell(button, wholeRow) {
	const text = wholeRow ? rowJSON(openCell.parentElement) : cellValue(openCell);
	if (!(await copy(button, text))) return;
	document.querySelector("#cell").close();
	document.querySelector("#notice").textContent = wholeRow ? "Copied the row as JSON" : "Copied the value";
}
const copyPlan = (button) => copy(button, button.dataset.plan);
const copyFrom = (button) => copy(button, button.dataset.copy);

async function copy(button, text) {
	let copied = true;
	try {
		await navigator.clipboard.writeText(text);
	} catch {
		// The clipboard API needs a secure context, which a webview's scheme may not count as.
		const scratch = document.createElement("textarea");
		scratch.value = text;
		(button.closest("dialog") ?? document.body).append(scratch);
		scratch.select();
		copied = document.execCommand("copy");
		scratch.remove();
	}
	const label = button.textContent;
	button.textContent = copied ? "Copied" : "Copy failed";
	setTimeout(() => (button.textContent = label), 1000);
	return copied;
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
	await openSQL(res.ok ? text : `-- ${text.trim().replaceAll("\n", "\n-- ")}`);
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

Object.assign(window, { toolOpen, showSidebar, fromMenu, newTab, closeActiveTab, selectTab, closeTab, closeAllTabs, useSQL, useDefinition, resetTree, formatSQL, showHistory, chooseHistory, sortHistory, showQueries, useQuery, openQueryFile, saveQuery, showActivity, showHealth, activityLive, showBackups, showRestore, closeRestore, showReplace, checkReplace, closeReplace, copyCell, updateCell, copyPlan, copyFrom });

// Wails holds back the menu's commands until its runtime says the page is ready.
// This page doesn't load that runtime, so it says so itself.
window.webkit?.messageHandlers?.external?.postMessage("wails:runtime:ready");

document.addEventListener("click", (event) => {
	const td = event.target.closest?.(".results tr[data-row] td");
	if (td && !event.target.closest("button") && getSelection().isCollapsed) showCell(td);
});

// The values given for each tab's :names, for htmx:configRequest to use. Export
// asks before its Save dialog, so they have to wait for its second request.
const paramValues = new Map();

// check asks the server about sql before it runs. If it can't, the query
// itself fails too, so there's nothing to warn about.
async function check(sql) {
	try {
		const response = await fetch("/check", { method: "POST", body: new URLSearchParams({ sql }) });
		return response.ok ? await response.json() : {};
	} catch {
		return {};
	}
}

// <details> menus only close by clicking their own button, unlike a real menu.
const closeMenus = (except) => {
	for (const menu of document.querySelectorAll("details.dropdown[open]")) {
		if (!menu.contains(except)) menu.open = false;
	}
};
document.addEventListener("click", (event) => closeMenus(event.target));
document.addEventListener("keydown", (event) => event.key === "Escape" && closeMenus());

// Sends the selection rather than the whole editor, and what was typed for History.
document.addEventListener("htmx:configRequest", (event) => {
	const { elt, formData, path } = event.detail;
	if (!formData.has("sql")) return;
	const sql = sqlToRun(elt);
	formData.set("sql", fillParams(sql, paramValues.get(elt.closest("[data-tab]").dataset.tab) ?? {}));
	if (path === "/query" && sql.trim()) formData.set("typed", sql.trim());
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

// Only what started the timer stops it: choosing where to export finishes just
// after the export itself has started.
document.addEventListener("htmx:afterRequest", (event) => {
	const timer = event.detail.elt.closest?.("[data-tab]")?.querySelector(".timer");
	if (timer && event.detail.requestConfig?.formData?.has("sql")) clearInterval(timer.interval);
});

// A History note saves as its box loses focus, so the box shows that it did,
// briefly, or that it didn't, until it does.
document.addEventListener("htmx:afterRequest", (event) => {
	const box = event.detail.elt;
	if (!box.matches?.("#history-detail textarea[name=note]")) return;
	const saved = event.detail.successful;
	box.classList.toggle("textarea-error", !saved);
	box.classList.toggle("textarea-success", saved);
	if (saved) setTimeout(() => box.classList.remove("textarea-success"), 1500);
});

// Runs before htmx sends a query, for the ones worth a second thought.
document.addEventListener("htmx:confirm", (event) => {
	const { elt, triggeringEvent } = event.detail;
	if (elt.dataset?.confirm) {
		event.preventDefault();
		ask(elt.dataset.confirm).then((ok) => ok && event.detail.issueRequest(true));
		return;
	}
	const isQuery = elt.matches("form[hx-post='/query']");
	if (!isQuery && !elt.matches("[hx-post='/export/choose']")) return;
	const sql = sqlToRun(elt);
	const names = paramNames(sql);
	if (!isQuery && names.length === 0) return;
	event.preventDefault();
	(async () => {
		const tab = elt.closest("[data-tab]").dataset.tab;
		const values = names.length > 0 ? await askParams(names) : {};
		if (!values) return;
		if (isQuery) {
			// Filled in, as :names aren't SQL the server can parse.
			const { everyRow } = await check(fillParams(sql, values));
			const warning = warningFor(triggeringEvent?.submitter?.value, everyRow);
			if (warning && !(await ask(warning))) return;
		}
		paramValues.set(tab, values);
		event.detail.issueRequest(true);
	})();
});

htmx.onLoad((elt) => {
	mountNewPanels();
	drawIcons(elt);
});
document.addEventListener("DOMContentLoaded", () => {
	if (localStorage.getItem("sidebar") === "queries") showSidebar("queries");
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

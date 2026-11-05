// The SQL editors, query tabs and tree actions. Bundled into web/static/app.js
// by `npm run build`.
import { basicSetup, EditorView } from "codemirror";
import { PostgreSQL, sql } from "@codemirror/lang-sql";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { Compartment, Prec } from "@codemirror/state";
import { keymap, placeholder } from "@codemirror/view";
import { tags } from "@lezer/highlight";

const views = new Map(); // tab id to its editor
const language = new Compartment();
let schema = {};
let activeTab;
const MIN_PANE = 80; // the smallest a dragged pane gets; what's beside it keeps twice this, as it holds the buttons too

const sqlLanguage = () => sql({ dialect: PostgreSQL, schema, defaultSchema: "public" });

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
			Prec.highest(keymap.of([{ key: "Mod-Enter", run: () => (textarea.form.requestSubmit(), true) }])),
			basicSetup,
			language.of(sqlLanguage()),
			theme,
			syntaxHighlighting(highlight),
			placeholder("select 1;  (⌘/Ctrl+Enter to run)"),
			EditorView.updateListener.of((update) => {
				if (update.docChanged) textarea.value = update.state.doc.toString();
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
	await fetch(`/tabs/${id}`, { method: "DELETE" });
	views.get(id).destroy();
	views.delete(id);
	document.querySelector(`[data-tab="${id}"]`).remove();
	document.querySelector(`[data-tab-button="${id}"]`).remove();
	if (activeTab === id) selectTab([...views.keys()].at(-1));
}

function mountNewPanels() {
	for (const panel of document.querySelectorAll("[data-tab]")) {
		if (!views.has(panel.dataset.tab)) mount(panel);
	}
}

// Shows text in the current tab if it's empty, otherwise in a new one, so
// browsing the tree never loses SQL.
async function openSQL(text) {
	if (views.get(activeTab).state.doc.length > 0) {
		await htmx.ajax("POST", "/tabs", { target: "#panels", swap: "beforeend" });
		mountNewPanels();
	}
	const view = views.get(activeTab);
	view.dispatch({ changes: { from: 0, insert: text } });
	view.focus();
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
	schema = await (await fetch("/completions")).json();
	for (const view of views.values()) view.dispatch({ effects: language.reconfigure(sqlLanguage()) });
}

function resetTree() {
	document.querySelector("#tree-search").value = "";
	loadSchema();
}

// Drag bars, rather than CSS resize: WebKit's corner handle can't be relied on
// with an editor filling the box. Each sizes the element just before it.
function resize(bar, size) {
	const box = bar.previousElementSibling;
	const prop = bar.dataset.resize;
	const room = prop === "height" ? box.parentElement.clientHeight : box.parentElement.clientWidth;
	box.style[prop] = `${Math.max(MIN_PANE, Math.min(size, room - 2 * MIN_PANE))}px`;
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

Object.assign(window, { selectTab, closeTab, useSQL, useDefinition, resetTree });

htmx.onLoad(mountNewPanels);
document.addEventListener("DOMContentLoaded", loadSchema);
document.addEventListener("connected", resetTree);

// Loaded blocking in <head> so a saved theme applies before first paint.
// With nothing saved, data-theme stays unset and the OS preference applies.
const savedTheme = localStorage.getItem("theme");
if (savedTheme) document.documentElement.dataset.theme = savedTheme;

function toggleTheme() {
	const current = document.documentElement.dataset.theme
		|| (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
	const next = current === "dark" ? "light" : "dark";
	document.documentElement.dataset.theme = next;
	localStorage.setItem("theme", next);
}

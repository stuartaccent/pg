# WebKit performance notes

What we learned in November 2026 making pg usable with large results. On macOS the window is WKWebView, so these are WebKit's costs. Windows uses WebView2 (Chromium), which hasn't been timed.

The test was 3,000 rows of 32 columns, about 96,000 cells. Times are to the next frame, on the real page in a WKWebView but not in the pg window itself, and are medians of a few runs on one Mac. Read them as sizes, not exact figures.

## The rule behind most of it

Anything that restyles or lays out every result cell takes about half a second at this size, and some actions did it twice. The cost is the number of cells, so it doesn't show on small results. Four things do it:

- changing an inherited value, custom properties included, on the root or any other ancestor of the results
- making the results inert, which `showModal()` does to everything behind it
- `display: none` on an ancestor, and back
- repainting them all, when they aren't isolated

## What was slow, and the fix

| Action | Before | After | Cause | Fix |
|---|---|---|---|---|
| Highlighting a row, or any change near the results | 6–7 s | 0.04–0.07 s | every cell repainted | `isolation: isolate` on `main.results` |
| Resizing a pane, per step | 6 s, then 0.5 s | 0.03–0.04 s | the same repaint, then a size variable on the root | `isolate`, then set the size on the panes (`resize`) |
| Opening a cell | 1.3 s | 0.06 s | daisyUI's scroll lock, and `showModal()` | exclude the scroll lock, and use `openDialog` |
| Other dialogs | 0.8 s | 0.05–0.07 s | `showModal()` | `openDialog` |
| Switching tabs | 0.7 s | 0.07 s | `display: none` | move tabs behind off screen (`setBehind`) |
| Scrolling, per frame | 25 ms | 17 ms | a sticky cell in every row | pin only the highlighted row's number |

The 6–7 s repaint was there before row numbers were added. It didn't show in a test page with only the table in it: **time the real page**.

We know `isolate` stops the repaint, but not exactly why WebKit repaints without it. `contain: paint`, `contain: layout paint` and `contain: strict` worked as well. `isolate` is the smallest.

## Rules for changes

- Don't set custom properties on `:root` from JavaScript. Set them on the element that uses them.
- Don't call `showModal()`. Use `openDialog`, which does what a modal does: Escape, Tab staying inside, focus pulled back, and one dialog at a time.
- Don't use `hidden` or `display: none` on a query panel. `setBehind` moves it off screen and marks it `aria-hidden`.
- Don't put `position: sticky` on a cell in every row.
- Keep `isolation: isolate` on `main.results`.
- Row numbers are a `::before` on the row, not a column. `app.js` finds columns by `cellIndex`, and a real column would shift them.

## Surprises

- **Focus:** after `dialog.show()`, WebKit left the focus in the editor, and after `showModal()` on the page body, so "Enter means Cancel" didn't work. `openDialog` focuses the first control itself.
- **Tabs behind are still there.** Without `display: none`, Tab, VoiceOver, Select All and a long drag could all reach them. Their controls are `visibility: hidden`, the panel is `aria-hidden`, and a `selectionchange` listener keeps a selection in the panel showing. The rows aren't made invisible, as that restyles each one.
- **`[hidden]` can't be overridden:** Tailwind's preflight sets it with `!important` in a layer, which beats our unlayered CSS. That's why tabs behind are styled by `aria-hidden`.
- **daisyUI's `exclude` needs 5.8.2 or later.** On 5.7.46, excluding `rootscrolllock` and `rootscrollgutter` leaves the `:root:has(.modal[open])` rules behind, still setting the variable.
- **`inert`** took about 1 s on a query panel, so it can't be used on one to hide it from the keyboard.

## Left alone

- **Changing theme, 0.5 s:** every cell really does change colour.
- **Highlighting a row, about 60 ms:** most of it is pinning that row's number.
- **Typing, about 40 ms a key against 19 ms with no results:** removing `:has()` rules made no difference, and removing position rules such as `:first-child` didn't make the frame any quicker.
- **Several huge tabs at once:** each stays laid out. With two, switching between them took 160 ms, opening or closing a cell 146 ms and typing 53 ms a key. More than two hasn't been timed.
- **Drawing only the rows on screen:** the real fix if results get much bigger. Hiding all but 500 rows took dialogs to 0.2 s and resizing to 0.1 s, before the fixes above. It's a big change: column widths, Copy row and row numbers rely on every row being there.

## Didn't help

- `content-visibility: hidden` on a hidden tab's results: 660 ms, against 700 ms for `display: none`.
- `table-layout: fixed`, for resizing once the sizes were off the root.
- `overflow: clip` on cells.

## Not tested

Windows, VoiceOver, the oldest macOS that pg runs on, and submitting a real Restore or Replace through `openDialog`.

## Timing it again

Playwright's Chromium is right for clicking through, but it isn't WebKit. Serve the page with `go run ./cmd/dev`, then load it in a visible WKWebView and run a script in it:

```swift
// swiftc -O time.swift -o time && ./time http://127.0.0.1:8080/ script.js
import Cocoa
import WebKit
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let frame = NSRect(x: 50, y: 50, width: 1700, height: 1000)
let win = NSWindow(contentRect: frame, styleMask: [.titled], backing: .buffered, defer: false)
let config = WKWebViewConfiguration()
config.websiteDataStore = .nonPersistent()
let web = WKWebView(frame: frame, configuration: config)
win.contentView = web
win.level = .floating
win.orderFrontRegardless() // hidden windows don't draw frames
let args = CommandLine.arguments
Task { @MainActor in
    web.load(URLRequest(url: URL(string: args[1])!))
    try? await Task.sleep(nanoseconds: 2_500_000_000)
    do {
        let script = try String(contentsOfFile: args[2], encoding: .utf8)
        print(try await web.callAsyncJavaScript(script, arguments: [:], in: nil, contentWorld: .page) ?? "nil")
    } catch { print(error) }
    exit(0)
}
app.run()
```

In the script, time to the frame after the change, not just the JavaScript. The 6 s repaint came after the script and layout were done:

```js
const raf = () => new Promise((r) => requestAnimationFrame(r));
async function time(change) {
	await raf();
	await raf();
	const start = performance.now();
	change();
	document.body.offsetHeight; // style and layout
	const script = performance.now() - start;
	await raf();
	await new Promise((r) => { // after the paint
		const channel = new MessageChannel();
		channel.port1.onmessage = r;
		channel.port2.postMessage(0);
	});
	return [script, performance.now() - start];
}
```

Frames are 17 ms at 60 Hz, so anything at 17 is as fast as this can show. Put the baseline in between variants: an early run without that gave misleading results.

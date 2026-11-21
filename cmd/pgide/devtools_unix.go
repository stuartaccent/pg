//go:build !windows && !production

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

static void enableDeveloperExtras(void *window) {
	dispatch_async(dispatch_get_main_queue(), ^{
		WKWebView *webView = [(NSWindow *)window valueForKey:@"webView"];
		[webView.configuration.preferences setValue:@YES forKey:@"developerExtrasEnabled"];
	});
}
*/
import "C"

import "github.com/wailsapp/wails/v3/pkg/application"

// enableDevTools turns on WebKit's developer extras, which View > Open Developer
// Tools and right-click > Inspect Element need. On macOS 13.3+ Wails only makes
// the webview inspectable from Safari, so its menu item does nothing without this.
func enableDevTools(window *application.WebviewWindow) {
	C.enableDeveloperExtras(window.NativeWindow())
}

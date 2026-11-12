#!/bin/sh
# Builds dist/pg.app from cmd/pgide. It's a folder with a set layout, so this is
# all it takes: macOS reads the name and icon from it before the app even runs.
set -eu
cd "$(dirname "$0")/.."

app=dist/pg.app
version=${VERSION:-dev} # the release tag without its "v"; "dev" builds don't update
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

go build -ldflags "-X main.version=$version" -o "$app/Contents/MacOS/pg" ./cmd/pgide

iconset=$(mktemp -d)/icon.iconset
mkdir "$iconset"
for size in 16 32 128 256 512; do
	sips -z "$size" "$size" cmd/pgide/icon.png --out "$iconset/icon_${size}x${size}.png" >/dev/null
	double=$((size * 2))
	sips -z "$double" "$double" cmd/pgide/icon.png --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil --convert icns --output "$app/Contents/Resources/icon.icns" "$iconset"

cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>pg</string>
	<key>CFBundleDisplayName</key>
	<string>pg</string>
	<key>CFBundleExecutable</key>
	<string>pg</string>
	<key>CFBundleIdentifier</key>
	<string>uk.co.accentdesign.pg</string>
	<key>CFBundleIconFile</key>
	<string>icon</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$version</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
PLIST

# Apple silicon won't run an unsigned app, and changing the bundle voids the
# signature the linker gave the binary. "-" signs it for this machine only.
codesign --force --deep --sign - "$app"

echo "built $app"

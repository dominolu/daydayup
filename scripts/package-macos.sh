#!/usr/bin/env bash
# Build a minimal, locally-runnable macOS .app bundle for DayDayUp.
#
# This is an ad-hoc signed development bundle. Distribution still needs a
# Developer ID Application certificate, hardened runtime and notarization.
set -euo pipefail
cd "$(dirname "$0")/.."

./scripts/build-desktop.sh

APP="dist/DayDayUp.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp bin/daydayup "$APP/Contents/MacOS/DayDayUp"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>
  <string>DayDayUp</string>
  <key>CFBundleDisplayName</key>
  <string>DayDayUp</string>
  <key>CFBundleIdentifier</key>
  <string>dev.daydayup.gateway</string>
  <key>CFBundleExecutable</key>
  <string>DayDayUp</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>NSPrincipalClass</key>
  <string>NSApplication</string>
  <key>CFBundleShortVersionString</key>
  <string>0.1.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
  <key>LSMinimumSystemVersion</key>
  <string>13.0</string>
  <key>LSUIElement</key>
  <false/>
  <key>NSHighResolutionCapable</key>
  <true/>
  <key>NSAppTransportSecurity</key>
  <dict>
    <key>NSAllowsLocalNetworking</key>
    <true/>
  </dict>
  <key>NSHumanReadableCopyright</key>
  <string>DayDayUp</string>
</dict>
</plist>
PLIST

if [[ -f desktop/packaging/AppIcon.icns ]]; then
  cp desktop/packaging/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
  /usr/libexec/PlistBuddy -c "Add :CFBundleIconFile string AppIcon" "$APP/Contents/Info.plist"
fi

# Ad-hoc signing is enough for local execution. Re-sign nested code first.
codesign --force --sign - "$APP/Contents/MacOS/DayDayUp"
codesign --force --sign - "$APP"
xattr -cr "$APP" 2>/dev/null || true

echo "built $APP"
if [[ "${1:-}" == "--open" ]]; then
  open "$APP"
fi

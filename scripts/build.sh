#!/bin/zsh
# Single build entry point. Repairs are local to this project/process.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
REPAIR=1 CHECK_ONLY=0
fail() { print -u2 "error: $*"; exit 1; }
while (( $# )); do
  case "$1" in
    --check) CHECK_ONLY=1 ;;
    --no-repair) REPAIR=0 ;;
    -h|--help)
      cat <<'HELP'
Usage: ./scripts/build.sh [--check] [--no-repair]

Build the Go CLI and SwiftUI App, bundle resources, and verify ad-hoc signing.
Every build clears project Swift/UI caches; Go module/build caches are retained.
  --check      Check/repair prerequisites, then exit without building.
  --no-repair  Do not select another Xcode or download a missing/old Go toolchain.

Automatic repair (default): select a compatible installed Xcode for this process,
install a checksum-verified Go toolchain under .tools/ if needed, and create caches.
Go selection: MACCLUSTER_GO, .tools/go/bin/go, PATH, then official Go download.
An explicit invalid MACCLUSTER_GO is an error, not silently replaced.
Xcode installation, Apple license acceptance, and administrator changes are manual.
No service restart, cluster change, commit, or upload is performed.
HELP
      exit 0 ;;
    *) fail "unknown option: $1 (use --help)" ;;
  esac
  shift
done
[[ "$(uname -s)" == Darwin ]] || fail 'This App must be built on macOS.'
[[ -f "$ROOT/go.mod" ]] || fail 'go.mod is missing. Run from a complete MacCluster checkout.'
if (( ! CHECK_ONLY )); then
  for source_file in app/Package.swift app/Sources/MacCluster/MacCluster.swift app/Resources/AppIcon.svg app/Resources/KubernetesIconWhite.png app/Resources/KubernetesIconWhite.source.txt; do
    [[ -f "$ROOT/$source_file" ]] || fail "$source_file is missing. Restore the project sources before building; environment repair does not restore deleted source files."
  done
fi

# A compatible installed Xcode can be used without changing global xcode-select.
compatible_xcode() {
  local sdk swift_version
  sdk="$(xcrun --sdk macosx --show-sdk-version 2>/dev/null)" || return 1
  [[ "${sdk%%.*}" == <-> ]] && (( ${sdk%%.*} >= 26 )) || return 1
  swift_version="$(xcrun swift --version 2>/dev/null)" || return 1
  [[ "$swift_version" =~ 'Swift version ([0-9]+)' ]] || return 1
  (( match[1] >= 6 ))
}
if ! compatible_xcode; then
  if (( REPAIR )); then
    for candidate in /Applications/Xcode*.app/Contents/Developer(N) "$HOME"/Applications/Xcode*.app/Contents/Developer(N); do
      if (export DEVELOPER_DIR="$candidate"; compatible_xcode); then
        export DEVELOPER_DIR="$candidate"
        print "Using installed Xcode: ${candidate:h:h:t} (this build only)"
        break
      fi
    done
  fi
  compatible_xcode || fail 'Install/open Xcode 26 or newer with macOS 26 SDK and Swift 6+, complete its first-launch setup, then rerun. Command Line Tools alone may be insufficient. You can set DEVELOPER_DIR to a compatible Xcode.'
fi
for tool in swift vtool; do xcrun --find "$tool" >/dev/null || fail "Xcode tool missing: $tool. Complete Xcode first-launch setup."; done
for tool in codesign curl shasum tar; do command -v "$tool" >/dev/null || fail "Required system tool missing: $tool"; done
PYTHON="$(xcrun --find python3 2>/dev/null || command -v python3 || true)"
[[ -n "$PYTHON" ]] || fail 'Python 3 is required to validate Go download metadata. Complete Xcode setup.'
print "macOS SDK: $(xcrun --sdk macosx --show-sdk-version)"

REQUIRED_GO="$(awk '$1 == "go" {print $2; exit}' go.mod)"
go_compatible() {
  local version
  [[ -x "$1" ]] || return 1
  version="$(GOTOOLCHAIN=local "$1" version 2>/dev/null)" || return 1
  "$PYTHON" - "$version" "$REQUIRED_GO" <<'PY'
import re,sys
m=re.search(r'\bgo(\d+)\.(\d+)(?:\.(\d+))?(?=\s)',sys.argv[1])
required=tuple(map(int,sys.argv[2].split('.')))
sys.exit(0 if m and tuple(int(v or 0) for v in m.groups()) >= required else 1)
PY
}
GO=''
if [[ -n "${MACCLUSTER_GO:-}" ]]; then
  GO="$(command -v "$MACCLUSTER_GO" 2>/dev/null || true)"
  go_compatible "$GO" || fail "MACCLUSTER_GO must point to a working Go $REQUIRED_GO+ executable. Correct or unset it to allow automatic setup."
else
  for candidate in "$ROOT/.tools/go/bin/go" "$(command -v go 2>/dev/null || true)"; do
    if go_compatible "$candidate"; then GO="$candidate"; break; fi
  done
fi
mkdir -p "$ROOT/.tools" "$ROOT/dist"
DOWNLOAD_DIR=''
AGENT_TEMP=''
TOOLS_TEMP=''
cleanup() {
  [[ -z "$DOWNLOAD_DIR" ]] || rm -rf -- "$DOWNLOAD_DIR"
  [[ -z "$AGENT_TEMP" ]] || rm -f -- "$AGENT_TEMP"
  [[ -z "$TOOLS_TEMP" ]] || rm -f -- "$TOOLS_TEMP"
  return 0
}
trap cleanup EXIT
if [[ -z "$GO" ]]; then
  (( REPAIR )) || fail "Go $REQUIRED_GO+ not found; rerun without --no-repair for project-local installation."
  case "$(uname -m)" in arm64) ARCH=arm64 ;; x86_64) ARCH=amd64 ;; *) fail 'Unsupported Mac architecture.' ;; esac
  DOWNLOAD_DIR="$(mktemp -d "$ROOT/.tools/go-download.XXXXXX")"
  print "Installing Go $REQUIRED_GO+ locally (official download, SHA256 verified)..."
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2 --connect-timeout 15 --max-time 120 'https://go.dev/dl/?mode=json' -o "$DOWNLOAD_DIR/releases.json"
  "$PYTHON" - "$DOWNLOAD_DIR/releases.json" "$ARCH" "$REQUIRED_GO" > "$DOWNLOAD_DIR/selection" <<'PY'
import json,re,sys
required=tuple(map(int,sys.argv[3].split('.')))
for release in json.load(open(sys.argv[1])):
    version=release.get('version','')
    m=re.fullmatch(r'go(\d+)\.(\d+)\.(\d+)',version)
    if not release.get('stable') or not m or tuple(map(int,m.groups())) < required:
        continue
    for f in release['files']:
        if f.get('os')=='darwin' and f.get('arch')==sys.argv[2] and f.get('kind')=='archive':
            expected=version+'.darwin-'+sys.argv[2]+'.tar.gz'
            if f['filename']!=expected or not re.fullmatch(r'[a-f0-9]{64}',f['sha256']):
                raise SystemExit('Invalid official download metadata')
            print(expected);print(f['sha256']);raise SystemExit(0)
raise SystemExit('No compatible stable Go archive found; install Go manually or retry later.')
PY
  GO_ARCHIVE="$(sed -n '1p' "$DOWNLOAD_DIR/selection")"
  GO_SHA="$(sed -n '2p' "$DOWNLOAD_DIR/selection")"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --retry 2 --connect-timeout 15 --max-time 600 "https://go.dev/dl/$GO_ARCHIVE" -o "$DOWNLOAD_DIR/go.tar.gz"
  [[ "$(shasum -a 256 "$DOWNLOAD_DIR/go.tar.gz" | awk '{print $1}')" == "$GO_SHA" ]] || fail 'Go checksum mismatch; archive will not be installed.'
  tar -xzf "$DOWNLOAD_DIR/go.tar.gz" -C "$DOWNLOAD_DIR"
  go_compatible "$DOWNLOAD_DIR/go/bin/go" || fail 'Downloaded Go cannot execute on this Mac.'
  if [[ -e "$ROOT/.tools/go" ]]; then
    GO_BACKUP="$(mktemp -d "$ROOT/.tools/go-backup.XXXXXX")"
    mv "$ROOT/.tools/go" "$GO_BACKUP/go"
    print "Previous project toolchain retained under ${GO_BACKUP#$ROOT/}"
  fi
  mv "$DOWNLOAD_DIR/go" "$ROOT/.tools/go"
  GO="$ROOT/.tools/go/bin/go"
fi
# The App and bundled agent must target the same host; never silently package Linux/cross builds.
[[ "$("$GO" env GOOS)" == darwin && "$("$GO" env GOARCH)" == "$("$GO" env GOHOSTARCH)" ]] || fail 'Unset GOOS/GOARCH cross-compilation overrides before building the Mac App.'
export GOPATH="$ROOT/.tools/gopath" GOMODCACHE="$ROOT/.tools/gomod" GOCACHE="$ROOT/.tools/gocache"
mkdir -p "$GOPATH" "$GOMODCACHE" "$GOCACHE"
print "Go: $(GOTOOLCHAIN=local "$GO" version)"
if (( CHECK_ONLY )); then print 'Prerequisites ready.'; exit 0; fi

# Always build the UI from scratch. These are generated Swift-only directories;
# never remove .tools (Go modules/compiler cache/toolchain) or unrelated .local data.
UI_BUILD_DIR="$ROOT/app/.build"
print 'Clearing Swift/UI build caches; retaining Go module and build caches...'
for ui_cache in "$UI_BUILD_DIR" "$ROOT/.local/swift-build" "$ROOT/.local/sidebar-build"; do
  rm -rf -- "$ui_cache"
done
export CLANG_MODULE_CACHE_PATH="$UI_BUILD_DIR/clang-module-cache"
export SWIFTPM_MODULECACHE_OVERRIDE="$UI_BUILD_DIR/swift-module-cache"
mkdir -p "$CLANG_MODULE_CACHE_PATH" "$SWIFTPM_MODULECACHE_OVERRIDE"
print 'Building Go agent and local tools (missing dependencies are downloaded automatically)...'
AGENT_TEMP="$(mktemp "$ROOT/dist/.maccluster-agent.XXXXXX")"
"$GO" build -trimpath -o "$AGENT_TEMP" ./cmd/maccluster-agent
mv -f "$AGENT_TEMP" "$ROOT/dist/maccluster-agent"
TOOLS_TEMP="$(mktemp "$ROOT/dist/.maccluster-tools.XXXXXX")"
"$GO" build -trimpath -ldflags '-X main.commandRole=tools' -o "$TOOLS_TEMP" ./cmd/maccluster-agent
# SwiftPM records the deployment target (14.0) as the SDK version the binary was linked against.
# macOS 26 only gives a binary the Liquid Glass look of toolbars, sidebars, sheets, segmented
# controls and default buttons when it was linked against the macOS 26 SDK or newer, so state the real
# SDK version while keeping macOS 14 as the minimum. Check with: vtool -show-build MacCluster
# MACCLUSTER_LINK_SDK stamps an older SDK on purpose (e.g. 26.0 when built with the macOS 27 SDK) to tell whether a
# layout difference comes from the new SDK's behavior or from the code; leave it unset for a normal build.
SDK_VERSION="${MACCLUSTER_LINK_SDK:-$(xcrun --sdk macosx --show-sdk-version)}"
LINK_FLAGS=(-Xlinker -platform_version -Xlinker macos -Xlinker 14.0 -Xlinker "$SDK_VERSION")
SWIFT_BUILD_ARGS=(--package-path "$ROOT/app" --scratch-path "$UI_BUILD_DIR"
  --cache-path "$UI_BUILD_DIR/swiftpm-cache" --disable-dependency-cache
  -c release --disable-sandbox --manifest-cache none -debug-info-format none "${LINK_FLAGS[@]}")
xcrun swift build "${SWIFT_BUILD_ARGS[@]}"
BIN_DIR="$(xcrun swift build "${SWIFT_BUILD_ARGS[@]}" --show-bin-path)"
# A binary stamped with an old SDK silently gets the pre-Liquid-Glass look of every system control.
LINKED_SDK="$(xcrun vtool -show-build "$BIN_DIR/MacCluster" | awk '/ sdk /{print $2; exit}')"
if (( ${SDK_VERSION%%.*} >= 26 && ${LINKED_SDK%%.*} < 26 )); then
  print -u2 "error: MacCluster is linked against SDK $LINKED_SDK, so macOS 26 would not draw its toolbar, sidebar and controls with Liquid Glass (installed SDK: $SDK_VERSION)."
  exit 1
fi
APP="$ROOT/dist/MacCluster.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$ROOT/app/Resources/KubernetesIconWhite.png" "$APP/Contents/Resources/KubernetesIconWhite.png"
cp "$ROOT/app/Resources/KubernetesIconWhite.source.txt" "$APP/Contents/Resources/KubernetesIconWhite.source.txt"
cp "$BIN_DIR/MacCluster" "$APP/Contents/MacOS/MacCluster.new"
mv -f "$APP/Contents/MacOS/MacCluster.new" "$APP/Contents/MacOS/MacCluster"
if [[ -x "$ROOT/dist/maccluster-agent" ]]; then
  cp "$ROOT/dist/maccluster-agent" "$APP/Contents/Resources/maccluster-agent.new"
  mv -f "$APP/Contents/Resources/maccluster-agent.new" "$APP/Contents/Resources/maccluster-agent"
else
  print -u2 "Warning: dist/maccluster-agent missing; app will show an unavailable-agent error until bundled."
fi
# Internal App component, not a separately distributed command.
mv -f "$TOOLS_TEMP" "$APP/Contents/Resources/maccluster-tools"
rm -f -- "$ROOT/dist/maccluster-tools" # remove the former standalone build artifact
# App icon. app/Resources/AppIcon.svg is the source; AppIcon.icns is generated from it by
# scripts/make-icon.sh and committed (with the SVG's hash). A changed SVG is picked up on every
# build: locally the icns is regenerated first; on CI (no GUI for QuickLook) a stale icns is an error.
ICON_SVG="$ROOT/app/Resources/AppIcon.svg"
ICON_ICNS="$ROOT/app/Resources/AppIcon.icns"
ICON_SUM="$ROOT/app/Resources/AppIcon.sha256"
if [[ -f "$ICON_SVG" ]]; then
  want="$(shasum -a 256 "$ICON_SVG" | cut -d' ' -f1)"
  have="$(cat "$ICON_SUM" 2>/dev/null || true)"
  if [[ ! -f "$ICON_ICNS" || "$want" != "$have" ]]; then
    if [[ -n "${CI:-}" ]]; then
      print -u2 "error: AppIcon.icns is stale for AppIcon.svg. Run scripts/make-icon.sh and commit the result."
      exit 1
    fi
    "$ROOT/scripts/make-icon.sh"
  fi
fi
if [[ -f "$ICON_ICNS" ]]; then
  cp "$ICON_ICNS" "$APP/Contents/Resources/AppIcon.icns.new"
  mv -f "$APP/Contents/Resources/AppIcon.icns.new" "$APP/Contents/Resources/AppIcon.icns"
else
  print -u2 "Warning: app/Resources/AppIcon.icns missing; the app will use the generic icon."
fi
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>MacCluster</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundleIdentifier</key><string>com.maccluster.desktop</string>
<key>CFBundleName</key><string>MacCluster</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSLocalNetworkUsageDescription</key><string>MacCluster 需要連線至區域網路的 Kubernetes 控制平面，以註冊節點並管理工作。</string>
</dict></plist>
PLIST
# Ad-hoc signing is sufficient for this local development build. Re-sign after
# replacing resources; copying over a live Mach-O can invalidate cached pages.
codesign --force --deep --sign - "$APP"
codesign --verify --deep --strict "$APP"
# Make Finder and the Dock pick up a changed icon instead of a cached one.
touch "$APP"
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$APP" >/dev/null 2>&1 || true
print "Built $APP"

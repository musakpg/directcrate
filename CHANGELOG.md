# Changelog

All notable changes to DirectCrate are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-11-14

### Initial Production Release

DirectCrate v1.0.0 is the initial production release of the high-performance, single-binary remote archive streamer. It streams and decompresses archives directly to disk over HTTP/HTTPS without ever storing intermediate `.rar`, `.zip`, `.7z`, or `.tar` archive files on local storage.

### Key Capabilities

#### Zero-Temp-Disk Streaming Engine
- **Direct-to-Disk Extraction**: Byte-range decompression in memory via `io.CopyBuffer` with configurable `sync.Pool` I/O buffer recycling (1 MB–16 MB) and 256 KB buffered writers, requiring 50% less storage than traditional downloaders.
- **Universal Format Support**:
  - **RAR5** (`.rar`, `.part1.rar`, `.r00`): Sequential streaming decompressor via `rardecode/v2` with multi-part URL chaining and mid-stream disk checks.
  - **ZIP** (`.zip`): Remote Central Directory range parsing with selective file tree filtering; downloads only requested files.
  - **7Z** (`.7z`): Remote 7Z metadata header inspection with selective file filtering and AES-256 decryption support.
  - **TAR / TAR.GZ / TAR.ZST**: High-performance streaming decompression (`archive/tar`, `gzip`, `zstd`) across single and multi-part archives.
- **Network Resilience**:
  - Parallel HTTP Range prefetching across 1–8 concurrent workers (default 4) for servers supporting `Accept-Ranges: bytes`.
  - Automatic TCP socket reconnection with exponential backoff on dropped connections.
  - Expiring CDN URL hot-swapping (`needs_url` state on HTTP 403/410) allowing instant resume without discarding already extracted files.
  - Atomic token-bucket bandwidth throttle (`Unlimited`, `5`, `10`, `20`, `35`, `50` MB/s) with mid-stream adjustments.
  - Sequential batch FIFO task queue with overnight power management actions.

#### Cross-Platform OS Integrations

##### Android 15 & 16
- **Native Architecture**: Pure Go engine compiled to 16KB-aligned `libdirectcrate.so` (`-extldflags=-Wl,-z,max-page-size=16384`) with JNI bridge and dynamic port collision handling.
- **Live Notification Shade Progress Bar**: Foreground service notification updating real-time percentage, transfer speed, and progress bar (`setProgress 0-100`).
- **Interactive Notification Actions**: Added one-tap "Abort" and "Stop / Exit" action buttons directly in the Android notification shade.
- **Smart Thermal Warning Notification**: Emits background notification alerts if device thermals enter hot/critical states during long decompression sessions.
- **Clean Idle Lifecycle**: Overrode `onTaskRemoved` to ensure swiping DirectCrate out of Recent Apps cleanly stops the foreground service and removes notification when idle, while keeping active extractions running safely in the background.
- **Native File Manager & DocumentsUI Integration**: Updated `openFolder` in `AndroidBridgeInterface` to launch the device's native DocumentsUI or default File Manager directly into the extracted folder via `DocumentsContract.buildDocumentUri` and `Intent.ACTION_VIEW` with `vnd.android.document/directory` and `resource/folder` fallbacks, keeping `ACTION_OPEN_DOCUMENT_TREE` strictly for folder browsing.
- **Proactive Storage Permission Flow**: Tapping "Browse" now proactively validates `MANAGE_EXTERNAL_STORAGE` and presents `#storagePermModal` immediately if ungranted, ensuring a smooth folder selection experience.
- **Native Bridges**: Safe Android clipboard integration (`readClipboard()`), native document tree directory picker (`OpenDocumentTree`), external browser URL launcher (`openExternalUrl()`), edge-to-edge system bar contrast synchronization (`WindowInsetsControllerCompat`), and `PowerManager` / `WifiLock` acquisition.

##### Linux / SteamOS / Bazzite
- **Standalone App-Mode Window**: Detects native Chromium, Chrome, Brave, Edge, or Flatpak browsers and launches with `--app=<url>` in a dedicated window without browser bars.
- **Wayland Taskbar Identity**: Automatically generates standard XDG desktop entry (`directcrate.desktop`) with space-safe quoted `Exec=` paths and registers high-resolution Hicolor scalable SVG icon.
- **Taskbar Progress Bar**: Emits D-Bus `com.canonical.Unity.LauncherEntry` signals to render dock/taskbar progress on KDE Plasma and GNOME.
- **Steam & Steam Deck Integration**: Non-Steam Game shortcut injection into `shortcuts.vdf` with automated cover art retrieval from Steam Store CDN (vertical capsule, hero banner, logo).
- **Sleep & Idle Prevention**: Acquires `systemd-inhibit` lock during active extraction.

##### Windows 10 & 11
- **Embedded Application Icon**: Branded isometric icon embedded directly into `directcrate-windows-amd64.exe` as a PE resource (`.syso`), displaying official iconography in File Explorer, Taskbar, and Alt+Tab.
- **Start Menu Shortcut**: Automatically registers `%APPDATA%\Microsoft\Windows\Start Menu\Programs\DirectCrate.lnk` with `AppUserModelID` asynchronously in the background.
- **Taskbar Progress (`ITaskbarList3`)**: Renders green running and yellow paused taskbar progress states via COM interface.
- **Sleep Prevention**: Thread execution state inhibitor prevents Windows sleep during active downloads.

#### Storage Integrity & Safety Safeguards
- **Cross-Filesystem Path Sanitizer**: Rejects relative path traversal attempts (`../`), strips illegal characters (`:`, `*`, `?`, `"`, `<`, `>`, `|`), trims trailing spaces/dots, and proactively prefixes Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) with `_` to prevent Win32 filesystem errors.
- **Pre-Flight Write & Margin Probes**: Validates directory write permissions with an ephemeral test file and verifies available storage against remote `Content-Length` (`+1 GB` safety buffer).
- **Mid-Stream Storage Limit Checks (`needs_disk`)**: Verified across all 4 extractors (RAR5, TAR, ZIP, 7Z) with automatic 2-second filesystem polling and resume once 512 MB is cleared.
- **Smart Directory Hoisting**: Automatically detects and hoists redundant double-nested directories (`Game/Game/...` → `Game/...`).
- **Automated Checksum Verification**: Automatically detects and verifies `.sha256`, `sha256sums.txt`, `.md5`, and `md5sums.txt` checksum files upon extraction completion.
- **Corrupted JSON Recovery**: Automatically backs up corrupted `settings.json` or `history.json` to `.corrupt.bak` and loads safe defaults without crashing.

#### Modern Web UI & Usability
- **Adaptive Studio Grid & Mobile Bottom Sheets**:
  - Desktop 2-column Studio Grid layout (`>= 860px`) separating controls and inputs on the left from live telemetry and queue on the right, with uniform 640px wide by 72vh centered modal dialogs across Settings, History, Logs, and Directory Picker.
  - Mobile-tailored layout (`< 860px`) with fixed thumb-friendly bottom action bar, uniform 78dvh slide-up bottom sheets with drag handles, and non-overflowing 2x2 multi-button grid footers preventing any action button clipping.
- **Embedded Zero-Dependency Frontend**: Single HTML/CSS/JS file embedded into the Go executable via `//go:embed`.
- **60fps High-DPI Telemetry & Live Statistics**: Canvas transfer speed sparkline rendered with `window.devicePixelRatio` scaling, live peak & running average speed metrics, and stream health counter (storage space saved, active streams, TCP retries).
- **Live Hardware Telemetry & Thermal Protection**: Real-time display of process memory (`App RAM`) and physical hardware temperature (`Device Temp`) with non-blocking smart thermal alerts and contextual quick-action throttling.
- **Per-Task Resource Footprint Summaries**: Continuous sampling of average and peak App RAM (`m.Sys`) and Device Temperature (`°C`) throughout active extractions, displaying compact resource metrics (`RAM: 113 MB avg (126 MB peak) • Temp: 36.1°C avg (37.2°C peak)`) in completion summaries, extraction history records, and system logs.
- **Conditional Auto-Marquee for Long File Paths**: Live file path display automatically enables a smooth CSS ping-pong scroll animation only when text overflows container width, keeping short paths static and readable.
- **GitHub Release Update Checker**: Background release checker with semantic version comparison, dismissible update notification banner, and manual update verification under Settings -> About.
- **Interactive Help Center & FAQ Redesign**:
  - Replaced static text with an interactive Help Center in Settings -> FAQ tab.
  - Sticky search bar (`#faqSearchInput`) with live counter and 1-tap "Reset Search & Filters" empty state.
  - Quick-jump topic chips (`All`, `Privacy & Safety`, `Storage & Folders`, `Links & Errors`, `Multi-Part & Passwords`, `Performance & Thermals`, `UI & Telemetry`, `Steam & Desktop`).
  - Animated accordion cards with category badges, chevron indicators, and keyboard-friendly ARIA expansion (`aria-expanded`).
- **Built-In On-Device Offline Support Assistant**:
  - Dedicated 4th tab in Settings ("Ask Help") running 100% locally in JavaScript with zero cloud telemetry or external models.
  - Contextual error and thermal alert detection with 1-tap diagnostic prompt.
  - Common troubleshooting starter chips and instant deterministic intent engine.
  - Actionable shortcut buttons directly in chat bubbles (`[Open HTTP Headers]`, `[Switch to Balanced 15 MB/s]`, `[Switch to Multi-Part Mode]`, `[Jump to FAQ Answer]`, `[View Diagnostics & Logs]`).
- **Privacy & Design Transparency Card**: Integrated into Settings -> About tab documenting zero-cloud, direct-to-disk architectural guarantees.
- **Accessible UI & Keyboard Navigation**: Custom theme-matched dropdown controls and modal dialogs with full ARIA accessibility (`role="dialog"`, `aria-modal="true"`, `aria-labelledby`), touch-friendly targets, and Escape key dismissal.
- **Plain-Language User Guidance**: In-app FAQ and beginner guide explaining 100% on-device privacy, storage savings, thermal behavior, and link requirements.

#### Automated Test Suite & Quality Assurance
- **Comprehensive Backend Unit & Integration Tests**: Full coverage in `main_test.go` covering API routes, probe fallbacks, token-bucket throttling, multi-part readers, archive inspection, path sanitization, corrupt file recovery, and graceful exit signals.
- **Automated Headless Browser E2E Test Suite**: Automated Chromium-based tests in `ui_e2e_test.go` simulating user interactions, modal dismissal, clipboard paste, theme changes, responsive viewports (Desktop 1280x800, Mobile 390x844, and Narrow Mobile 360x800), automated geometric bounds verification ensuring zero button clipping and strict 2px modal height consistency, and verifying zero JavaScript console errors and 100% ARIA compliance.
- **7 Cross-Platform UI/UX Validation Criteria**: Verified zero horizontal overflow across all panes, touch target sizing (`>= 40px`), safe-area padding, viewport containment, keyboard/back navigation, light/dark theme contrast, and zero console errors.
- **Atomic Race Detection**: 100% test pass rate with `go test -race` and `CGO_ENABLED=0` cross-compilation.


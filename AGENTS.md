# Windows Screenshot MCP Server — LLM Usage Guide

## Purpose
Use the `windows-screenshot` MCP server to visually explore, understand, and interact with Windows applications you have no prior knowledge of.

## Available Tools

### list_windows
List all visible windows with title, class, PID, handle, size, and state.
```json
{"method": "title", "target": "..."}  // no args needed
```
**Use first** to discover what's running and get exact window titles.

### take_screenshot
Capture a window by title, PID, handle, or class name.
```json
{"method": "title", "target": "App Name", "format": "png"}
```
- `method`: `title` | `pid` | `handle` | `class`
- `format`: `png` (default) | `jpeg`
- Returns an error if the window is too small (<200×100) or minimized.

### capture_desktop
Capture the full desktop/primary monitor. No arguments needed. Useful for seeing overall window layout.

### control_window
Control a window's state, position, or size.
```json
{"method": "title", "target": "App Name", "action": "restore"}
```
Actions:
- `restore` — restore a minimized window
- `maximize` — maximize to full screen
- `minimize` — minimize to taskbar
- `focus` — bring to foreground
- `move` — reposition (requires `x`, `y`)
- `resize` — change size (requires `width`, `height`)
- `move_resize` — set both (requires `x`, `y`, `width`, `height`)

## Workflow: Exploring an Unknown Application

### 1. Discover
```
list_windows → find the app's exact title, class, PID, size, state
```

### 2. Prepare
```
control_window → restore/maximize/focus if minimized or hidden
```

### 3. Overview
```
take_screenshot → capture the main window to see the full UI layout
```
Describe what you see: menus, toolbars, sidebar, panels, status bar, tabs.

### 4. Explore Systematically
For each visible section, tab, or menu:
- Note what you see in the current screenshot
- Identify interactive elements (buttons, tabs, menus, inputs)
- Request the user to click/navigate to the next section, then screenshot again
- Build a map of the application's structure

### 5. Record Findings
After exploration, document:
- **Application**: name, version, framework (infer from window class)
- **UI structure**: menus, panels, tabs, toolbars
- **Key features**: what each section does
- **Connections/devices**: any network or hardware interfaces visible
- **Status indicators**: what telemetry or state is shown

## Tips
- **Always `list_windows` first** — exact titles matter; partial matches won't work.
- **Always ensure the window is visible** before capturing — use `control_window` with `restore` or `focus`.
- **Qt apps** (class starts with `Qt`) may need fallback capture methods; the server handles this automatically.
- **Minimized windows** report as ~160×28 (title bar only) and will be rejected by `take_screenshot` with a helpful error.
- **Use `capture_desktop`** when you need to see where windows are positioned relative to each other.
- **Window class names** reveal the framework: `Qt5*` = Qt, `Chrome_WidgetWin_1` = Electron/Chrome, `Afx:*` = MFC, `CabinetWClass` = File Explorer.
- The MCP server runs at `http://127.0.0.1:8080/mcp` (Streamable HTTP transport, JSON-RPC 2.0).

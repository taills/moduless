# Air Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create a `.air.toml` configuration file in the project root to support hot-reloading of the Go Core Gateway (`core/main.go`) during development.

**Architecture:** Create `.air.toml` at the repository root, configured to watch `core/` and `proto/` folders, compile the server binary to `tmp/main`, and execute it with clean up on exit.

**Tech Stack:** Go, Air.

## Global Constraints
- Configuration file must be placed at the project root: `.air.toml`.
- Air must build `./core/main.go` and output binary to `./tmp/main`.

---

### Task 1: Create and Verify Air Configuration

**Files:**
- Create: `.air.toml`

**Interfaces:**
- Produces: Live reload capability for the Core Gateway when `air` command is run from root.

- [ ] **Step 1: Create `.air.toml` configuration file**

Create `.air.toml` in the repository root directory with the following content:

```toml
# Config file for Air (https://github.com/cosmtrek/air) in TOML format

root = "."
tmp_dir = "tmp"

[build]
  cmd = "go build -o ./tmp/main ./core/main.go"
  bin = "./tmp/main"
  full_bin = "./tmp/main"
  include_ext = ["go", "tpl", "tmpl", "html", "yaml", "yml", "proto"]
  exclude_dir = [
    "assets", "tmp", "vendor", "tests", "docs", 
    "sdk/python", "sdk/java", "extension-example", 
    ".git", ".pytest_cache"
  ]
  include_dir = ["core", "proto"]
  exclude_file = []
  exclude_regex = ["_test.go"]
  exclude_unchanged = false
  follow_symlink = false
  log = "air.log"
  poll = false
  poll_interval = 0
  delay = 1000 
  stop_on_error = true
  send_interrupt = true
  kill_delay = 500

[log]
  time = true

[color]
  main = "magenta"
  watcher = "cyan"
  build = "yellow"
  runner = "green"

[misc]
  clean_on_exit = true
```

- [ ] **Step 2: Check if `air` builds the binary successfully**

Run: `air -v` to verify if Air is installed. If installed, run `air` and verify that the binary is built under `./tmp/main` and starts correctly.
Expected: Air starts, builds the application, and the gateway initializes.

- [ ] **Step 3: Commit the changes**

Run:
```bash
git add .air.toml
git commit -m "feat: add air configuration for live-reloading core gateway"
```

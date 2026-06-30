# Air Configuration Design Specification (Core Gateway)

This document specifies the configuration for using [Air](https://github.com/cosmtrek/air) to support live reloading (hot compile and run) during Go Core Gateway development.

## Objective
Enable smooth local development for the Go Core Gateway (`core/main.go`) by automatically recompiling and restarting the server whenever code inside `core/` or protobuf contracts inside `proto/` change.

## Design

We will place a `.air.toml` configuration file in the project root directory.

### Configuration Content

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

### Verification Criteria
1. The `.air.toml` file is created at the repository root.
2. Running `air` in the repository root builds `core/main.go` and runs it.
3. Modifying a Go file in `core/` triggers a rebuild.
4. Modifying non-gateway files (e.g., Python SDK files) does NOT trigger a rebuild.

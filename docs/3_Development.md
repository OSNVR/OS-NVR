### Index

- [Program Map](#program-map)
- [The Addon system](#the-addon-system)
- [Running The CI Suite](#running-the-ci-suite)
- [The Video Server](../pkg/video/README.md)

<br>

## Program Map

```
.
├── nvr.go   # Main app.
├── addon.go # Addon hooks.
├── addons
│   ├── auth
│   │   ├── basic/
│   │   └── none/
│   └── thumbscale/
├── start
│   ├── build/main.go # Build file output.
│   └── start.go      # Start script.
├── pkg
│   ├── ffmpeg
│   │   ├── ffmock/   # ffmpeg sub-process mock.
│   │   └── ffmpeg.go # ffmpeg helper functions.
│   ├── group # Monitor groups.
│   ├── log
│   │   ├── db.go  # Log storage.
│   │   └── log.go # Logging.
│   ├── monitor
│   │   ├── monitor.go
│   │   └── recorder.go
│   ├── storage
│   │   ├── crawler.go   # Finds recordings.
│   │   ├── storage.go
│   │   ├── types.go
│   │   └── video.go
│   ├── system/
│   ├── video/ # Internal Video server.
│   └── web
│       ├── auth/     # Authentication definitions.
│       ├── routes.go # HTTP handlers.
│       └── web.go    # Templating.
├── go.mod       # Go Dependencies.
├── package.json # Optional front-end tools.
├── utils
│   ├── ci-fmt.sh # Format, lint and test.
│   └── services/ # Service scripts.
└── web # Front-end.
    ├── static
    │   ├── icons/
    │   ├── scripts/
    │   └── style/
    └── templates
        ├── includes # Sub-templates/Nested templates.
        │   └── sidebar.tpl
        └── live.tpl

```


<br>

## The Addon System

The addon system is inspired by [Caddy](https://caddyserver.com/docs/architecture). It injects imports statements into a `main.go` build file and `go` runs it as a sub-process. The addons use `init()` to register hooks in the app before it's started.

This is done by the [start script](./start/start.go) at runtime.


#### Minimal example.

```
.
├── start
│   └── main.go
├── addons
│   ├── A
│   │   └── A.go
│   └── B
│       └── B.go
└── app.go
```

```
// main.go
import (
	app
	_ app/addons/A
	_ app/addons/B
)

// main() is called after packages have been imported.
func main() {
	app.Start()
}

```

```
// A.go
import app

// This is called when main.go imports it.
func init() {
	// Register message in app.
	app.registerMsg("a")
}
```

```
// B.go
import app

func init() {
	app.registerMsg("b")
}
```

```
// app.go
package app

var messages []string
func RegisterMsg(msg string) {
	messages = append(messages, msg)
}

func Start() {
	for _, msg := range messages {
		println(msg)
	}
}
```


See the simple [thumbscale](./addons/thumbscale/thumb.go) addon.

<br>

## Running The CI Suite

The repository is configured to automatically run the CI suite on all pull request and prevent merging until it passes.

### Nix shells

Install the Nix package manager: https://nixos.org/download
```
nix-shell ./utils/nix/shell.nix
./utils/ci-fmt.sh
```

<br>

### Docker
```
docker run -it -v $(pwd):/app osnvr/os-nvr_ci:v0.14.0
nix-shell # The CI image uses Nix shells internally.
cd /app
GOFLAGS=-buildvcs=false ./utils/ci.sh
# You only need the buildvsc flag if Docker is running as root.
```

<br>

### Manual Install

All the tools are listed in `./utils/nix/shell.nix`. Run `npm install` in the project root after node and npm have been installed.

Run full CI suite: `./utils/ci-fmt.sh`
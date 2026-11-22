# rpi-ssh-go

An MCP server that gives an AI assistant a shell on your Raspberry Pis through [Raspberry Pi Connect](https://connect.raspberrypi.com). No open SSH port, no VPN and no browser: the server signs in to Connect and talks to the device's Remote Shell directly over WebRTC.

It is a single Go binary that speaks the [Model Context Protocol](https://modelcontextprotocol.io) over stdio, so it works with Claude Code, Claude Desktop, Antigravity and any other MCP client.

> **Unofficial.** This project is not affiliated with or endorsed by Raspberry Pi Ltd. It uses the same web endpoints as the Connect website, which are not a published API and can change without notice.

## What you get

| Tool | What it does |
| :--- | :--- |
| `ssh_list_devices` | Lists the devices on your Connect account: name, online state, remote shell availability, client version. |
| `ssh_exec` | Runs a command in a persistent bash session on a device and returns its output and exit code. |
| `ssh_upload_file` | Writes a text or binary file to a device, verified with SHA-256 and published atomically. |
| `ssh_download_file` | Reads a file from a device, text or binary, limited to the size you ask for. |
| `ssh_disconnect` | Closes the session to a device. |

Parameters, results and examples are in [docs/TOOLS.md](docs/TOOLS.md).

Things it takes care of:

- **A shell that keeps its state.** The working directory, variables and background jobs survive between calls, as in a terminal you leave open.
- **Commands that cannot wedge the session.** Quoting, syntax errors, unfinished constructs and prompts for input all come back as a result instead of hanging until the timeout.
- **Safe file transfer.** Uploads go to a staging file, are checked against a SHA-256 of the content and then renamed over the destination, so a failed transfer never leaves a half-written file in place.
- **Bounded everything.** Output, downloads, request sizes and concurrency are limited, so one large file or one chatty command cannot flood the assistant's context or take the server down.
- **No inbound connections.** By default all traffic leaves through Connect's TURN relays over TCP/TLS. Nothing listens on your machine and Windows Firewall does not prompt.

## How it works

1. The server signs in to Raspberry Pi Connect with your Raspberry Pi ID and keeps the login cookies.
2. For a device it has no session with, it creates a WebRTC offer, posts it to Connect and waits for the device's answer.
3. A data channel named `shell` opens to the Remote Shell on the Pi. Commands are typed into it wrapped in unique markers, and the output between the markers is returned with the exit code.
4. The session stays open and is reused until it is disconnected, a command times out, or the server exits.

## Requirements

- A Raspberry Pi running [Raspberry Pi Connect](https://www.raspberrypi.com/documentation/services/connect.html) with **Remote shell** enabled, signed in to your account. The remote shell must be bash with the usual GNU tools (`base64`, `stat`, `head`, `mktemp`, `sha256sum`), which is what Raspberry Pi OS provides.
- A Raspberry Pi ID **without two-factor authentication**. The server signs in with email and password and cannot enter a second factor.
- [Go](https://go.dev/dl/) 1.27.2 or newer to build.

The server is developed and tested on Windows. It also builds for Linux and macOS.

## Install

Build from source:

```bash
git clone https://github.com/ItxFilas/rpi-ssh-go.git
cd rpi-ssh-go
go build -ldflags="-s -w" -o rpi-ssh.exe .      # Windows
go build -ldflags="-s -w" -o rpi-ssh .          # Linux, macOS
```

On Windows you can use the build script instead, which also prints the size and SHA-256 of the binary:

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build.ps1            # builds dist\rpi-ssh.exe
powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -PackageZip # also writes dist\rpi-ssh-go.zip
```

## Credentials

The server needs the email and password of your Raspberry Pi ID. It looks for them in this order:

1. `RPI_EMAIL` and `RPI_PASSWORD` in the server's environment, when both are set.
2. The first of these files that defines both:
   - `.env` next to the executable
   - `~/.rpi-connect.env`
   - `~/.env`
   - `.env` in the working directory

A `.env` file looks like this:

```ini
RPI_EMAIL=you@example.com
RPI_PASSWORD=your-password
```

Values may be quoted, lines may start with `export`, and the file may be UTF-8 or UTF-16.

After the first sign-in the login is kept in `~/.rpi-connect-cookies.json`, so the password is only used again when that login expires. See [SECURITY.md](SECURITY.md) for how the file is protected.

## Add it to your MCP client

**Claude Code**

```bash
claude mcp add --scope user rpi-ssh -- /full/path/to/rpi-ssh
```

To pass the credentials through the client instead of a `.env` file:

```bash
claude mcp add --scope user --env RPI_EMAIL=you@example.com --env RPI_PASSWORD=your-password rpi-ssh -- /full/path/to/rpi-ssh
```

**Claude Desktop, Antigravity and other clients** use a JSON block in their MCP configuration:

```json
{
  "mcpServers": {
    "rpi-ssh": {
      "command": "C:\\Tools\\rpi-ssh\\rpi-ssh.exe",
      "args": [],
      "env": {
        "RPI_EMAIL": "you@example.com",
        "RPI_PASSWORD": "your-password"
      }
    }
  }
}
```

Leave `env` out if you use a `.env` file.

Then ask the assistant something like *"list my Raspberry Pis"* or *"check the disk space on kitchen-pi"*.

A client starts the server once and keeps it running. After replacing the binary, reconnect the server in the client (or restart the client) to load the new version.

## Network modes

Set `RPI_NETWORK_MODE` in the server's environment to choose how WebRTC connects.

| Mode | Behaviour |
| :--- | :--- |
| `outbound-relay` (default) | Only Connect's TURN relays over TCP/TLS are used. No local listeners, no mDNS, no firewall prompt, no administrator rights. If Connect offers no such relay the connection fails; there is no silent fallback. |
| `direct` | Normal WebRTC with direct host and UDP candidates. Faster when both ends can reach each other, but Windows Firewall may ask to allow access. |

To confirm that a running server has no listening sockets on Windows:

```powershell
scripts\verify-windows-network.ps1 -ProcessId <PID> -AssertOutboundOnly
```

## Behaviour worth knowing

- **A timeout closes the session.** When a command times out or the request is cancelled, the shell is discarded so that late output cannot leak into the next command. The next call opens a fresh shell: working directory and variables are reset. The error includes what the command had printed.
- **`timeout_seconds` covers the command only.** Connecting to a device with no open session is budgeted separately, up to 30 seconds.
- **Commands cannot read from the terminal.** Standard input is `/dev/null`, so a program that asks a question gets end-of-file and fails at once. `sudo` asks on the terminal itself and would wait; use `sudo -n`.
- **Output is merged.** The remote shell is a terminal, so standard error arrives together with standard output in `stdout`.
- **Background processes are yours to stop.** Closing a session does not guarantee that processes started with `&` or `nohup` end.
- **A dead connection takes a moment to notice.** A short network interruption does not close the session. It is dropped once WebRTC reports the connection as failed, which can take about 30 seconds.

Limits:

| What | Limit |
| :--- | :--- |
| Command timeout | 30 s by default, 1 to 300 s |
| Command output | 8,000 characters by default, up to 1,000,000; the middle of longer output is cut |
| Command length | 1 MiB; commands over roughly 2.5 KB are uploaded and run as a script |
| Upload size | 8 MiB |
| Download size | `max_chars` characters (15,000 by default, up to 1,000,000) of a file of any size |
| Concurrent tool calls | 8; further calls are rejected with `-32000` |
| Request size | 32 MB per message |

## Performance

Measured with version 2.0.0 on Windows (Go 1.27.2) against a Raspberry Pi over Connect. The figures have not been re-measured for 2.1.0, which sends more commands through the Base64 path.

| Measure | Result |
| :--- | :--- |
| Warm command, default relay mode | about 60 ms (median 60.6 ms, p95 62.3 ms) |
| Warm command, direct mode | about 4 ms |
| Cold connect | 3.7 to 25 s, including sign-in, device lookup, relay allocation and the WebRTC handshake |
| Memory (working set) | about 24 MB |
| Binary size | about 11.5 MB, stripped |

Collecting command output costs constant memory (about 77 KB per command) at roughly 80 MB/s, whatever the size of the stream.

## More documentation

- [docs/TOOLS.md](docs/TOOLS.md): every tool with parameters, results and examples
- [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md): error messages and what to do about them
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md): code layout, tests, how to debug a change in Connect
- [SECURITY.md](SECURITY.md): what the server can do, what it stores, how to report a problem
- [CHANGELOG.md](CHANGELOG.md): what changed in each version

## License

[MIT](LICENSE)

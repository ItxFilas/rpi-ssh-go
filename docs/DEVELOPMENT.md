# Development

## Layout

```text
main.go                 wiring: Connect client, session pool, MCP server on stdio
pkg/auth                sign-in to Raspberry Pi ID, credentials, cookie storage
pkg/connect             Connect web client: device list, shell page, offer and answer
pkg/rtc                 WebRTC sessions: pool, command execution, output parsing, upload stream
pkg/rtc/rtctest         test fixtures: in-process WebRTC peers, a bash behind a data channel
pkg/terminal            what gets typed into the shell: command envelope, upload receiver script
pkg/mcp                 JSON-RPC server, tool definitions, upload and download logic
scripts/                build and Windows network check
test_integration.py     end-to-end checks and benchmarks against a real device
```

How a tool call travels:

1. `pkg/mcp/server.go` reads one JSON-RPC message per line, validates the arguments against the tool's schema and runs the handler in its own goroutine (at most eight at a time).
2. The handlers in `pkg/mcp/tools.go` get a session from the pool (`pkg/rtc/pool.go`). The pool keeps one session per device and serialises connecting per device.
3. `Session.Exec` (`pkg/rtc/session.go`) wraps the command with `terminal.WrapCommand`, sends it over the data channel and feeds what comes back to the output collector (`pkg/rtc/output.go`), which finds the markers, strips terminal escapes and bounds the result.
4. One operation at a time uses a session. Commands, transfers and keepalives take the session's gate.

## Build

```bash
go build ./...
go vet ./...
```

`scripts\build.ps1` builds the stripped Windows binary into `dist\` and prints its SHA-256. With `-PackageZip` it also writes a ZIP and refuses to include `.env` files, cookie files or backup executables.

## Tests

```bash
go test ./...
```

Many tests run a real **bash**, because what the server does is type shell into a terminal and the shell is the only judge of that. They are skipped when `bash` is not in `PATH`. On Windows, install Git for Windows and run the tests from a shell that has its `bash` in `PATH`.

No test in the default run touches the network or opens a listening socket. WebRTC tests connect two peers through Pion's in-memory network (`pkg/rtc/rtctest`).

Useful fixtures:

- `rtctest.LoopbackPair` gives you two connected peers and both ends of a `shell` data channel.
- `rtctest.StartBashShell` puts a real bash behind one end. Together with `rtc.AttachSession` on the other end you get a session that behaves like one to a Raspberry Pi, without a Pi. `pkg/mcp/e2e_test.go` builds a whole pool from it and calls the tools through the JSON-RPC server.
- `pkg/connect/testdata/devices_page.html` is a reduced copy of the real devices page. Changes to the page parser are checked against it.

File modes and symbolic links cannot be observed through Git Bash on Windows, so the two tests that check them (`TestUploadKeepsModeOfReplacedFile`, `TestUploadWritesThroughSymlink`) are skipped there and run on Linux.

The shell commands the server sends use GNU options (`base64 -w`, `stat -c`, `mktemp -p`, `chmod --reference`, `mv -T`), because that is what a Raspberry Pi has. The tests that run them therefore need GNU tools: they pass on Linux and under Git Bash on Windows, and fail on macOS unless GNU coreutils come first in `PATH`. The server itself builds and runs on macOS.

### Continuous integration

`.github/workflows/ci.yml` runs on every push to `master` and on pull requests:

- `gofmt`, `go vet` and `go test ./...` on Linux and Windows, plus cross-builds for the other platforms.
- A separate job runs the tests with the race detector. It is allowed to fail for now, because the race detector had not been run on this code when the workflow was added.

### Against a real device

Two things talk to a real Raspberry Pi. Both need credentials as described in the [README](../README.md#credentials).

```bash
# Go test of the upload stream, behind a build tag
RPI_TEST_DEVICE=my-pi go test -tags integration -run TestLive ./pkg/rtc/

# End-to-end checks and benchmarks through the built binary
python test_integration.py --binary dist/rpi-ssh.exe --device my-pi
python test_integration.py --binary dist/rpi-ssh.exe --device my-pi --allow-write-tests
```

Without `--allow-write-tests` the script only runs read-only commands. With it, the script uploads and downloads files of up to 8 MiB in a temporary directory on the device.

## Conventions

- **A test first.** Bugs get a test that fails for the reason the bug exists, then the fix. Several tests in this repository are named after the failure they prevent.
- **Never leave the shell in an unknown state.** If an operation cannot tell what the remote side did (a timeout, a send error), close the session. A fresh shell is always correct; a reused one might not be.
- **Bound what comes back.** Anything that returns data from the device takes a limit and enforces it before the data reaches the MCP client.
- **Do not replay.** A command or a publish step whose outcome is unknown is reported as such, never sent again.

## When Connect changes

The server uses the endpoints of the Connect website. They are not a published API, so the usual cause of a sudden failure is that the site changed. The site's own JavaScript is the reference for what a request must look like.

To compare the server with the browser:

1. Fetch a device's shell page, `/devices/<id>/remote-shell-session`, with a signed-in session. The page carries the ICE configuration in `data-shell-ice-configuration-value` and links the script `shell_session-<hash>.js`.
2. In that script, find the request to `/devices/${deviceId}/connections`. Its method, headers and body are what `Client.PostOffer` in `pkg/connect/client.go` must send. The polling that follows corresponds to `Client.PollAnswer`.
3. For a response the server rejects, log the response body. Connect answers a malformed request with an HTML error page whose title names the problem.

Example: in November 2026 Connect began answering offers with `400 Bad Request`. The script showed the browser sending `Content-Type: application/json`, while the server sent `application/octet-stream`. `TestPostOfferSendsJSONContentType` now pins that.

For changes to the devices page, save the new page, reduce it, replace device names and IDs with made-up ones, and update `pkg/connect/testdata/devices_page.html` together with the parser.

## Adding a tool

1. Register it in `RegisterRPiTools` (`pkg/mcp/tools.go`) with a name, description and JSON schema. The schema's `required` list and property types are enforced before the handler runs.
2. Get the session with `pool.GetOrOpen(ctx, deviceName)`, then apply your own timeout to the work that follows. Connecting has its own budget.
3. Return text for the model to read. For structured results use `marshalResult`, which does not HTML-escape.
4. Test it through the server with the bash-backed pool, as `pkg/mcp/protocol_test.go` does.
5. Document it in [TOOLS.md](TOOLS.md).

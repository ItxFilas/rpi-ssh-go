# Changelog

All notable changes to this project are listed here. Versions follow [Semantic Versioning](https://semver.org).

## [2.1.0] - Unreleased

### Fixed

- **Connecting failed with `offer rejected by Raspberry Pi Connect (status 400)`.** Connect no longer accepts the WebRTC offer as `application/octet-stream`; it is now sent as JSON, as the Connect website does.
- **Commands hung until the timeout** when their output was not valid UTF-8 (Latin-1 text, binary data). Undecodable bytes are now shown as U+FFFD.
- **Commands hung, or re-ran the previous command,** when they contained `!`, `(`, `)`, `{`, `}` or ended in an open construct such as `if true`. Only commands made of plain words are typed as they are; everything else is Base64-framed.
- **`timeout_seconds` included the time to connect,** so short timeouts could never succeed on a device without an open session. It now covers the command only; connecting has its own budget of up to 30 seconds.
- **Uploaded files always had mode `0600`.** A file now gets the mode of the file it replaces, or the shell's default for new files.
- **Failed uploads left `.staging.*` files behind.** The receiver removes its own file when a transfer does not complete, and cleanup falls back to a fresh session when the transfer closed its own.
- **Downloading a file over 8 MiB failed and closed the session.** Only the bytes needed for `max_chars` are transferred now, whatever the size of the file.
- **`max_chars` was ignored for binary files,** which could return megabytes of base64. The base64 payload is limited as well.
- **Sign-in and network failures were reported as `device not found`.** The actual failure is reported.
- **Device names containing `&`, `'`, `<` or `"` could not be used,** because the name was taken from the page without decoding it.
- **`ssh_list_devices` reported a wrong client version** (for example `011.06.02`), read from an icon on the page instead of the version column.
- **Paths starting with `~/` failed** in uploads and downloads.
- **A request larger than the line limit made the server exit.** It is rejected and the server keeps running.
- Uploads to a symbolic link replaced the link instead of the file it points to.
- Deciding whether a freshly fetched device list could be reused compared two clock readings that are often equal on Windows, which fetched the page twice and made a test fail at random.

### Added

- `ssh_upload_file` accepts `content_base64` for binary files.
- `ssh_exec` reports `truncated` in its result.
- `ssh_exec` runs commands of up to 1 MiB. A command too long for one terminal line is uploaded and sourced in the same shell.
- A command that times out returns the output it printed until then.
- Binary downloads report `size_bytes`, `returned_bytes` and `truncated`; truncated text downloads state the file's full size.
- The server answers `ping`.
- `.env` files may be UTF-8 with a byte order mark or UTF-16, and lines may start with `export`.
- Sign-in errors say whether the account asks for a second factor or the credentials were not accepted.
- A page that lists devices in markup the server cannot read is reported as an error instead of an empty list.
- Documentation: tool reference, troubleshooting, development guide, security notes.
- MIT license.
- GitHub Actions workflow: formatting, vet and tests on Linux and Windows, and a race detector job.

### Changed

- **Commands read end-of-file on standard input.** A command that asks for input now fails at once instead of hanging until the timeout.
- **A brief loss of connectivity no longer closes the session.** It is closed once WebRTC reports the connection as failed, which can take about 30 seconds.
- Output that arrives while no command is running (background jobs) is discarded instead of filling the receive queue and closing the session.
- Connecting fails as soon as the connection dies, and waiting for the device's answer gives up after repeated error responses or a redirect to the sign-in page, instead of waiting out the full timeout.
- `RPI_EMAIL` and `RPI_PASSWORD` in the environment take precedence over `.env` files when both are set. Before, a `.env` file won.
- Tool results are JSON without HTML escaping: `<`, `>` and `&` in command output arrive as typed.
- The truncation note of `ssh_download_file` now reads `... [truncated to N characters; file size M bytes]`.
- Open sessions are closed on every exit path of the server.

### Security

- The login cookie file is encrypted for the current user on Windows (DPAPI). Plain files written by earlier versions still load and are rewritten encrypted. An earlier version cannot read the encrypted file and signs in again.
- Removed a credentials path that was hard-coded to the author's machine.

### Removed

- The unused `FileTransferManager` in `pkg/terminal`.

## [2.0.0]

The baseline for this changelog: native WebRTC through `pion/webrtc` without a browser, persistent sessions with a keepalive, bounded output streaming, transactional uploads with SHA-256 verification, and a relay-only network mode that opens no listening sockets. See the git history for details.

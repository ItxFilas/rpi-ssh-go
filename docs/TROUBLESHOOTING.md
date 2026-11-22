# Troubleshooting

Find the message you got below. If a fix does not seem to take effect, see [The server still behaves like the old version](#the-server-still-behaves-like-the-old-version) first.

## Signing in

### `session expired and RPI_EMAIL/RPI_PASSWORD not configured`

The saved login is gone or has expired and the server has no credentials to sign in again. Provide `RPI_EMAIL` and `RPI_PASSWORD` in the server's environment or in one of the `.env` files listed in the [README](../README.md#credentials).

The same message can appear inside another one, for example `could not look up device 'kitchen-pi': session expired ...`. The device name is not the problem there; the sign-in is.

### `authentication failed: the sign-in was not accepted, check RPI_EMAIL and RPI_PASSWORD`

Raspberry Pi ID rejected the email or password. Check both by signing in at <https://connect.raspberrypi.com> in a browser. If you use a `.env` file, make sure the server is reading the one you edited: environment variables take precedence, then the file next to the executable, then the ones in your home directory.

### `authentication failed: this Raspberry Pi ID uses two-factor authentication`

The account asks for a second factor after the password, which the server cannot enter. Use an account without two-factor authentication for the devices you want to reach this way.

### `initial csrf token not found in response` or `id provider csrf token not found in response`

The sign-in pages did not look as expected. Usually Raspberry Pi changed them. Check for a newer version of this server, and open an issue if there is none.

## Finding the device

### `device 'x' not found`

No device with that name or ID is on the account. Call `ssh_list_devices` with `force_refresh` set and use a name exactly as listed. Names are matched without regard to case.

### `device 'x' is currently offline`

The device is not connected to Raspberry Pi Connect. Check that it has power and network, and that the Connect service is running on it (`rpi-connect status`).

### `devices page markup not recognised`

The devices page on connect.raspberrypi.com links to devices but the server could not read the list. The page layout has changed. Check for a newer version of this server, and open an issue if there is none.

## Connecting

### `offer rejected by Raspberry Pi Connect (status 400)`

Versions before 2.1.0 sent the WebRTC offer with a content type that Connect no longer accepts (first seen in November 2026). Update to 2.1.0 or newer.

On a current version, a rejected offer means Connect changed what it expects again. See [docs/DEVELOPMENT.md](DEVELOPMENT.md#when-connect-changes) for how to compare the server's request with the browser's.

### `Raspberry Pi Connect did not provide a compatible TCP/TLS TURN relay`

In the default `outbound-relay` mode the server only connects through a TURN relay over TCP/TLS, and Connect did not offer one. Set `RPI_NETWORK_MODE=direct` in the server's environment to allow direct WebRTC connections. On Windows that may bring up a firewall prompt.

### `timed out waiting for WebRTC DataChannel to open` or `WebRTC connection failed before the DataChannel opened`

The device answered but no connection could be established.

- Check that **Remote shell** is enabled for the device (`rpi-connect shell on`) and that `ssh_list_devices` reports `"shell": true`.
- A firewall or proxy may be blocking the relay. Try the other network mode: `RPI_NETWORK_MODE=direct`, or remove it to go back to the relay.
- Try the Remote shell on the Connect website from the same machine. If that fails too, the problem is between the device and Connect.

### `timeout waiting for SDP answer from Raspberry Pi` or `SDP answer poll failed 5 times in a row`

Connect accepted the request but the device never answered. The device is probably going offline or its Connect service is stuck. Restart the service on the device (`rpi-connect restart`) or reboot it.

### `SDP answer poll was redirected to /sign-in`

The login expired in the middle of connecting. Call the tool again; the server signs in again if it has credentials.

## Running commands

### `Execution error: command execution cancelled or timed out`

The command was still running after `timeout_seconds`. The session was closed, so the next call starts in a new shell. The message includes what the command printed until then.

- Raise `timeout_seconds` (up to 300) if the command simply takes longer.
- If the command was waiting for a password, use `sudo -n` or configure passwordless sudo for it. Programs that need a real terminal for input (editors, `top`, anything that draws a screen) cannot be used.
- For long jobs, start them in the background and poll: `nohup ./job.sh > job.log 2>&1 &`, then `tail job.log` in later calls.

### The command returns immediately with an error instead of asking a question

That is intended. Commands read end-of-file on standard input, so `apt install` without `-y`, `read`, `rm -i` and similar fail at once instead of waiting for an answer nobody can give. Pass the option that answers the question (`-y`, `--force`) or pipe the answer in: `printf 'y\n' | some-command`.

### `command output exceeded receive buffer limit`

The command produced output faster than it could be collected and more than 1 MiB was waiting. The session was closed. Redirect large output to a file on the device and read the part you need with `ssh_download_file`, `tail` or `grep`.

### `command envelope exceeded line limit`

The command is longer than 1 MiB. Upload it as a file with `ssh_upload_file` and run that.

### `session is closed` or `session closed while waiting for command output`

The connection to the device ended, for example because the device rebooted, the command ran `exit`, or the network dropped for too long. Call the tool again to open a new session.

### `server busy: maximum 8 concurrent operations`

Eight tool calls are already running. Wait for one to finish.

## Transferring files

### `Upload error: failed to create staging file: ... No such file or directory`

The destination's directory does not exist. Create it first with `ssh_exec` (`mkdir -p ...`).

### `Upload error: failed to create staging file: ... Permission denied`

The session's user cannot write to that directory. Upload to a directory you own and move the file with `sudo -n mv`, if passwordless sudo is set up.

### `Upload error: destination '...' is an existing directory`

`remote_path` must name the file, not the directory it goes into.

### `upload publish outcome unknown`

The connection was lost at the moment the new file was being moved into place. The destination has either the old or the new content. Read it back, and upload again if needed.

### `staging file '...' residue may remain`

An upload failed and the server could not remove its temporary file afterwards. Delete the named `.staging.*` file on the device.

### `Download error: remote path '...' is a directory`

`ssh_download_file` reads one file. List the directory with `ssh_exec`.

### A text file comes back as JSON with `content_base64`

The file is not valid UTF-8, so it is treated as binary. For a text file in another encoding, convert it on the device: `iconv -f latin1 -t utf-8 file`.

## Other

### The server still behaves like the old version

An MCP client starts the server once and keeps that process. Replacing the binary on disk does not change a server that is already running. Reconnect the server in the client (in Claude Code: `/mcp`, choose the server, **Reconnect**) or restart the client. Each open client window has its own copy.

### Windows Firewall asks to allow access

That only happens in `direct` network mode. Remove `RPI_NETWORK_MODE` to use the default relay mode, which opens no listening sockets.

### `Invalid Request: message too large`

A single request was larger than 32 MB. Uploads are limited to 8 MiB of content, which fits well within that.

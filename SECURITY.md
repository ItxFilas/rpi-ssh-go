# Security

## What this server can do

Anyone who can call the tools of this server can run any command, and read and write any file, as the Remote Shell user on **every device of the Raspberry Pi Connect account** it is signed in to. With passwordless `sudo` on a device, that means root.

The MCP client decides which commands are run. Treat connecting this server to an assistant like handing it an open terminal on those devices:

- Keep the client's confirmation prompts on for `ssh_exec` and `ssh_upload_file` unless you trust what is driving it.
- Output from a device is passed to the assistant as text. A file or a log on a device can contain instructions meant to mislead it.
- Use a Connect account that only has the devices you want reachable this way.

## What it stores

| What | Where | Protection |
| :--- | :--- | :--- |
| Raspberry Pi ID email and password | The server's environment, or a `.env` file you create | None beyond the file's permissions. The server reads them and never writes them. |
| Login cookies | `~/.rpi-connect-cookies.json` | On Windows, encrypted for the current user account with the Data Protection API. Elsewhere, a file readable by its owner only. |

The password is sent only to `id.raspberrypi.com` during sign-in and is not included in error messages or tool results. Sign-in errors report the address a login ended at without its query string.

To sign the server out, delete `~/.rpi-connect-cookies.json`.

## Network

- In the default `outbound-relay` mode the server opens no listening sockets and uses only outbound TCP/TLS connections to Raspberry Pi's services and TURN relays. `scripts\verify-windows-network.ps1` checks this for a running process on Windows.
- In `direct` mode WebRTC also uses local UDP and host candidates, as a browser would.
- Traffic to the device is end-to-end encrypted by WebRTC (DTLS) in both modes.

## On the device

- Commands run in an interactive bash started by Raspberry Pi Connect. They appear in that shell's history according to the device's own settings.
- Uploads write a temporary `.staging.*` file next to the destination and remove it when the transfer does not complete. Long commands are written to a temporary file in the device's temp directory and removed after they run; if such a command times out, the file can remain.
- Closing a session does not stop background processes the session started.

## Limits of the protection

- A process running as your user on the machine that runs the server can read the `.env` file and, on Windows, decrypt the cookie file.
- Accounts with two-factor authentication are not supported, so the account used here is protected by its password alone.
- The server relies on the endpoints of the Connect website and trusts the responses it gets over TLS from `connect.raspberrypi.com` and `id.raspberrypi.com`.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Use **Report a vulnerability** under the repository's **Security** tab on GitHub (private vulnerability reporting), and include the version (`serverInfo.version` in the `initialize` response) and steps to reproduce.

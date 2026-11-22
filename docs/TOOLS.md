# Tool reference

The server exposes five tools. Every tool that works on a device takes `device_name`, which is matched against the device's name (ignoring case) or its ID as shown by `ssh_list_devices`.

A call either returns a result or an error text with `isError` set. Errors start with what failed: `Validation error:`, `Connection error:`, `Execution error:`, `Upload error:` or `Download error:`.

- [ssh_list_devices](#ssh_list_devices)
- [ssh_exec](#ssh_exec)
- [ssh_upload_file](#ssh_upload_file)
- [ssh_download_file](#ssh_download_file)
- [ssh_disconnect](#ssh_disconnect)

## ssh_list_devices

Lists the devices registered on the Raspberry Pi Connect account.

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `force_refresh` | boolean | `false` | Fetch the list now instead of using the copy cached for 30 seconds. |

Result:

```json
{
  "active_sessions": ["kitchen-pi"],
  "devices": [
    {
      "id": "22222222-aaaa-4bbb-8ccc-000000000002",
      "name": "kitchen-pi",
      "online": true,
      "shell": true,
      "version": "2.14.0"
    }
  ]
}
```

| Field | Meaning |
| :--- | :--- |
| `devices[].online` | The device is connected to Raspberry Pi Connect right now. |
| `devices[].shell` | Remote shell is enabled and available. It is only reported for devices that are online. |
| `devices[].version` | Version of the Connect client on the device. |
| `active_sessions` | Names of the devices this server has an open session with, or `null` when there are none. |

## ssh_exec

Runs a command in the device's bash session. The first call to a device opens the session; later calls reuse it, so the working directory, variables and background jobs carry over.

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `device_name` | string | required | Device to run on. |
| `command` | string | required | Bash command. Several lines, pipes, here-documents and quoting of any kind are fine. |
| `timeout_seconds` | integer | `30` | Time allowed for the command, 1 to 300. Connecting is not counted. |
| `max_output_chars` | integer | `8000` | Characters of output to return, 1 to 1,000,000. |

Result:

```json
{
  "device": "kitchen-pi",
  "exit_code": 0,
  "stdout": "Filesystem      Size  Used Avail Use% Mounted on\n/dev/mmcblk0p2   58G   11G   45G  20% /",
  "truncated": false
}
```

| Field | Meaning |
| :--- | :--- |
| `exit_code` | Exit status of the command. A syntax error in the command is reported here (bash uses 2), not as a tool error. |
| `stdout` | Output with terminal escape sequences removed and surrounding whitespace trimmed. Standard error is included, because the shell is a terminal. Bytes that are not valid UTF-8 appear as U+FFFD. |
| `truncated` | `true` when the output was longer than `max_output_chars`. The first 30% and the last 70% of the allowance are kept and a note in between says how much was left out. |

What to expect:

- **Input.** Commands read end-of-file on standard input. A command that asks a question fails immediately instead of waiting. `sudo` prompts on the terminal directly; use `sudo -n` so it fails when a password would be needed.
- **Long commands.** A command that does not fit on one terminal line (roughly 2.5 KB) is uploaded to a temporary file, sourced in the same shell and removed again. It behaves like any other command. The limit is 1 MiB.
- **Timeouts.** When the command is still running after `timeout_seconds`, the call fails and the session is closed, because output arriving later could not be told apart from the next command's. The next call opens a new shell with a fresh working directory and environment. The error text includes the output printed so far:

  ```text
  Execution error: command execution cancelled or timed out: context deadline exceeded (timeout_seconds was 3; the shell session was closed, so its working directory and variables are reset)

  Output before the command stopped:
  migration started
  ```

  The remote process is not guaranteed to stop. For long jobs, start them in the background (`nohup ... &`) and check on them with later calls.
- **`exit`.** Running `exit` ends the remote shell. The call fails with `session closed while waiting for command output`, and the next call opens a new session.

## ssh_upload_file

Writes a file on the device.

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `device_name` | string | required | Device to write to. |
| `remote_path` | string | required | Destination file. Absolute, relative to the shell's current directory, or starting with `~/`. |
| `content` | string | | Text content. |
| `content_base64` | string | | Content as base64, for binary files. |

Exactly one of `content` and `content_base64` must be given. An empty `content` creates an empty file. The content may be up to 8 MiB.

Result:

```text
Successfully uploaded 24 bytes to '/home/pi/deploy.sh' on kitchen-pi (SHA-256: 5c3c219c7eef).
```

How the file is written:

1. The destination is resolved to an absolute path. A symbolic link is followed, so the file it points to is replaced.
2. The content is streamed into a staging file (`.staging.XXXXXXXXXX`) in the destination's directory.
3. The staging file's SHA-256 is compared with the content's.
4. The staging file is given the mode of the file it replaces. A new file gets the shell's default mode (`0666` reduced by the umask).
5. The staging file is renamed over the destination.

The destination therefore either keeps its old content or has the complete new content. If anything fails before the rename, the staging file is removed. The directory must exist; it is not created.

If the connection is lost in the instant of the rename, the error says `upload publish outcome unknown`. The file may or may not have been replaced; read it back to find out.

## ssh_download_file

Reads a file from the device.

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `device_name` | string | required | Device to read from. |
| `remote_path` | string | required | File to read. Absolute, relative to the shell's current directory, or starting with `~/`. |
| `max_chars` | integer | `15000` | Characters to return, 1 to 1,000,000. |

Only the bytes needed for `max_chars` are transferred, so the size of the file does not matter. Reading the start of a 20 MB log takes as long as reading a small file.

**Text files** (valid UTF-8) are returned as text. When the file is longer than `max_chars`, the text ends with a note:

```text
... [truncated to 15000 characters; file size 20000000 bytes]
```

**Binary files** are returned as JSON:

```json
{
  "remote_path": "/bin/ls",
  "size_bytes": 199576,
  "returned_bytes": 150,
  "truncated": true,
  "content_base64": "f0VMRgIBAQAAAAAAAAAAAAMAtwABAAAA..."
}
```

`content_base64` holds the first `returned_bytes` bytes and is never longer than `max_chars` characters. To get a whole binary file, set `max_chars` to at least four thirds of its size; the ceiling of 1,000,000 characters corresponds to 750,000 bytes.

To read another part of a large file, use `ssh_exec` with `tail`, `sed -n` or `grep`.

## ssh_disconnect

Closes the session to a device. The remote shell ends, and with it the working directory and variables of that session.

| Parameter | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `device_name` | string | required | Device to disconnect from. |

Result:

```text
Disconnected persistent session for 'kitchen-pi'.
```

or `No active session found for 'kitchen-pi'.` when there was none. Sessions are also closed when the server exits.

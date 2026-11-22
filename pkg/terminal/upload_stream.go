package terminal

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var validTokenRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

const (
	MaxUploadBytes       = 8 * 1024 * 1024 // 8 MiB
	UploadAckInterval    = 32768           // 32 KiB
	MaxUploadPayloadLine = 1024            // 1,024 decoded bytes per base64 line
)

// UploadReceiverCommand generates a bounded bash command that receives streaming base64 upload lines.
// It sets up terminal restoration, disables echo, and coordinates using protocol markers.
func UploadReceiverCommand(stagingPath, token string, byteCount int) (string, error) {
	trimmedPath := strings.TrimSpace(stagingPath)
	if trimmedPath == "" {
		return "", errors.New("staging path cannot be empty")
	}

	trimmedToken := strings.TrimSpace(token)
	if !validTokenRe.MatchString(trimmedToken) {
		return "", errors.New("invalid upload token: must be 16-64 alphanumeric characters")
	}

	if byteCount < 0 {
		return "", errors.New("byteCount cannot be negative")
	}
	if byteCount > MaxUploadBytes {
		return "", fmt.Errorf("byteCount %d exceeds maximum limit of %d (8 MiB)", byteCount, MaxUploadBytes)
	}

	quotedStaging := ShellQuote(trimmedPath)

	// Keep the receiver alive after failure until the transport closes stdin.
	// Otherwise payload already in flight could be interpreted by the parent shell.
	// Unless the transfer completed, the exit trap removes the staging file:
	// after a failure or a dropped connection nobody else is left to do it.
	script := fmt.Sprintf(`(
__stty_save=$(stty -g 2>/dev/null || true)
__td=""
__b64_pid=""
__ok=0
__cleanup() {
  __ec=$?
  trap - EXIT INT TERM HUP PIPE
  if [ -n "$__b64_pid" ]; then
    kill "$__b64_pid" 2>/dev/null || true
    wait "$__b64_pid" 2>/dev/null || true
  fi
  if [ -n "$__td" ]; then rm -rf "$__td" 2>/dev/null || true; fi
  if [ "$__ok" != 1 ]; then rm -f -- %s 2>/dev/null || true; fi
  if [ -n "$__stty_save" ]; then stty "$__stty_save" 2>/dev/null || true; fi
  exit $__ec
}
__fail() {
  trap '' PIPE
  printf '\n__MCP_UP_DONE_%s_1__\n'
  cat >/dev/null
  exit 1
}
trap __cleanup EXIT
trap 'exit 1' INT TERM HUP
trap __fail PIPE
stty -echo 2>/dev/null || true
printf '\n__MCP_UP_READY_%s__\n'
__total=%d
if [ "$__total" -eq 0 ]; then
  : > %s || __fail
else
  __td=$(mktemp -d /tmp/_mcp_up.XXXXXXXXXX) || __fail
  mkfifo "$__td/f" || __fail
  base64 -d < "$__td/f" > %s &
  __b64_pid=$!
  exec 3> "$__td/f" || __fail
  __rec=0
  __accum=0
  while [ "$__rec" -lt "$__total" ]; do
    IFS= read -r __line || __fail
    __n=$(( __total - __rec ))
    if [ "$__n" -gt %d ]; then __n=%d; fi
    [[ $__line =~ ^[A-Za-z0-9+/]*={0,2}$ ]] || __fail
    __len=${#__line}
    __pad=0
    case "$__line" in *==) __pad=2 ;; *=) __pad=1 ;; esac
    [ $(( __len %% 4 )) -eq 0 ] && [ $(( __len / 4 * 3 - __pad )) -eq "$__n" ] || __fail
    printf '%%s\n' "$__line" >&3 || __fail
    __rec=$(( __rec + __n ))
    __accum=$(( __accum + __n ))
    if [ "$__accum" -ge %d ] || [ "$__rec" -eq "$__total" ]; then
      printf '\n__MCP_UP_ACK_%s_%%d__\n' "$__rec"
      __accum=0
    fi
  done
  exec 3>&-
  wait "$__b64_pid"
  __dec_rc=$?
  __b64_pid=""
  [ "$__dec_rc" -eq 0 ] || __fail
fi
__ok=1
printf '\n__MCP_UP_DONE_%s_0__\n'
)`, quotedStaging, trimmedToken, trimmedToken, byteCount, quotedStaging, quotedStaging,
		MaxUploadPayloadLine, MaxUploadPayloadLine, UploadAckInterval, trimmedToken, trimmedToken)

	b64Script := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := fmt.Sprintf("eval \"$(printf '%%s' '%s' | base64 -d)\"\n", b64Script)
	return cmd, nil
}

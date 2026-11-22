package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/rtc"
)

// DefaultDownloadChars is how many characters ssh_download_file returns
// unless max_chars says otherwise.
const DefaultDownloadChars = 15000

// marshalResult renders v as indented JSON for a tool result. HTML escaping
// is off: the text is read by a model, and unicode escapes in place of "<",
// ">" and "&" only obscure command output.
func marshalResult(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("failed to format result: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// uploadContent returns the bytes to upload from the tool arguments: text in
// "content" or arbitrary bytes as base64 in "content_base64".
func uploadContent(args map[string]any) ([]byte, error) {
	text, hasText := args["content"]
	encoded, hasEncoded := args["content_base64"]
	hasText = hasText && text != nil
	hasEncoded = hasEncoded && encoded != nil

	switch {
	case hasText && hasEncoded:
		return nil, fmt.Errorf("Validation error: provide only one of content and content_base64")
	case hasText:
		switch v := text.(type) {
		case string:
			return []byte(v), nil
		case []byte:
			return v, nil
		}
		return nil, fmt.Errorf("Validation error: content must be a string")
	case hasEncoded:
		v, ok := encoded.(string)
		if !ok {
			return nil, fmt.Errorf("Validation error: content_base64 must be a string")
		}
		// Line breaks are common in pasted base64.
		v = strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(v)
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("Validation error: content_base64 is not valid base64: %v", err)
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("Validation error: content or content_base64 must be provided")
}

func parseBoundedInt(val any, defaultVal, minVal, maxVal int, paramName string) (int, error) {
	if val == nil {
		return defaultVal, nil
	}
	var intVal int64
	switch v := val.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, fmt.Errorf("Validation error: %s must be an integer", paramName)
		}
		if v < float64(math.MinInt64) || v > float64(math.MaxInt64) {
			return 0, fmt.Errorf("Validation error: %s value overflows integer", paramName)
		}
		intVal = int64(v)
	case int:
		intVal = int64(v)
	case int64:
		intVal = v
	case int32:
		intVal = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("Validation error: %s must be a valid integer: %w", paramName, err)
		}
		intVal = parsed
	default:
		return 0, fmt.Errorf("Validation error: %s must be an integer", paramName)
	}

	if intVal < int64(minVal) || intVal > int64(maxVal) {
		return 0, fmt.Errorf("Validation error: %s must be between %d and %d (got %d)", paramName, minVal, maxVal, intVal)
	}
	return int(intVal), nil
}

func RegisterRPiTools(s *Server, client *connect.Client, pool *rtc.Pool) {

	// 1. ssh_list_devices
	s.RegisterContextTool(map[string]any{
		"name":        "ssh_list_devices",
		"description": "List all Raspberry Pi devices on connect.raspberrypi.com with status and remote shell availability.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"force_refresh": map[string]any{
					"type":        "boolean",
					"description": "Force live fetch instead of 30s cache",
					"default":     false,
				},
			},
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		if client == nil {
			return "Raspberry Pi Connect client is not initialized", true
		}
		if ctx.Err() != nil {
			return "Operation cancelled", true
		}

		forceRefresh := false
		if v, ok := args["force_refresh"].(bool); ok {
			forceRefresh = v
		}
		devices, err := client.ListDevices(ctx, forceRefresh)
		if err != nil {
			return fmt.Sprintf("Error listing devices: %v", err), true
		}

		activeSessions := []string{}
		if pool != nil {
			activeSessions = pool.ActiveSessionNames()
		}

		return marshalResult(map[string]any{
			"devices":         devices,
			"active_sessions": activeSessions,
		}), false
	})

	// 2. ssh_exec
	s.RegisterContextTool(map[string]any{
		"name":        "ssh_exec",
		"description": "Execute a shell command on a Raspberry Pi over native WebRTC Remote Shell with persistent shell state.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_name": map[string]any{
					"type":        "string",
					"description": "Name of the Raspberry Pi as listed by ssh_list_devices (e.g. 'kitchen-pi')",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Bash command to execute",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"description": "Timeout for the command in seconds (default 30). Connecting to a device with no open session is budgeted separately (up to 30s).",
					"default":     30,
				},
				"max_output_chars": map[string]any{
					"type":        "integer",
					"description": "Maximum characters before truncation (default 8000)",
					"default":     8000,
				},
			},
			"required": []string{"device_name", "command"},
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		devName, _ := args["device_name"].(string)
		cmd, _ := args["command"].(string)

		if strings.TrimSpace(devName) == "" {
			return "Validation error: device_name cannot be empty", true
		}
		if strings.TrimSpace(cmd) == "" {
			return "Validation error: command cannot be empty", true
		}

		timeoutSec, err := parseBoundedInt(args["timeout_seconds"], 30, 1, 300, "timeout_seconds")
		if err != nil {
			return err.Error(), true
		}

		maxChars, err := parseBoundedInt(args["max_output_chars"], 8000, 1, 1000000, "max_output_chars")
		if err != nil {
			return err.Error(), true
		}

		if pool == nil {
			return "WebRTC session pool is not initialized", true
		}

		if ctx.Err() != nil {
			return "Command execution cancelled", true
		}

		// Connecting is budgeted by the pool. timeout_seconds starts once
		// the session is ready, so a slow cold connect cannot use it up.
		session, err := pool.GetOrOpen(ctx, devName)
		if err != nil {
			return fmt.Sprintf("Connection error: %v", err), true
		}

		execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()

		if execCtx.Err() != nil {
			return "Command execution cancelled", true
		}

		result, err := session.Exec(execCtx, cmd, maxChars)
		if err != nil {
			msg := fmt.Sprintf("Execution error: %v", err)
			if errors.Is(err, context.DeadlineExceeded) {
				msg += fmt.Sprintf(" (timeout_seconds was %d; the shell session was closed, so its working directory and variables are reset)", timeoutSec)
			}
			// A command that did not finish still returns what it printed.
			if result.Stdout != "" {
				msg += "\n\nOutput before the command stopped:\n" + result.Stdout
			}
			return msg, true
		}

		return marshalResult(map[string]any{
			"device":    result.Device,
			"exit_code": result.ExitCode,
			"stdout":    result.Stdout,
			"truncated": result.Truncated,
		}), false
	})

	// 3. ssh_upload_file
	s.RegisterContextTool(map[string]any{
		"name":        "ssh_upload_file",
		"description": "Upload a file to the Raspberry Pi with chunked transfer and SHA-256 verification. Pass text as content, or any bytes as content_base64. The file keeps the mode of the one it replaces.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_name": map[string]any{
					"type":        "string",
					"description": "Name of the Raspberry Pi",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "File text content to write",
				},
				"content_base64": map[string]any{
					"type":        "string",
					"description": "File content as base64, for binary files (instead of content)",
				},
				"remote_path": map[string]any{
					"type":        "string",
					"description": "Destination file path on the Pi (absolute, relative to the shell's directory, or starting with ~/)",
				},
			},
			"required": []string{"device_name", "remote_path"},
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		devName, _ := args["device_name"].(string)
		remotePath, _ := args["remote_path"].(string)

		if strings.TrimSpace(devName) == "" {
			return "Validation error: device_name cannot be empty", true
		}
		if strings.TrimSpace(remotePath) == "" {
			return "Validation error: remote_path cannot be empty", true
		}

		contentBytes, err := uploadContent(args)
		if err != nil {
			return err.Error(), true
		}

		if pool == nil {
			return "WebRTC session pool is not initialized", true
		}

		if ctx.Err() != nil {
			return "Upload cancelled", true
		}

		// Connecting is budgeted by the pool; the transfer has its own deadline.
		session, err := pool.GetOrOpen(ctx, devName)
		if err != nil {
			return fmt.Sprintf("Connection error: %v", err), true
		}

		uploadCtx, cancel := context.WithTimeout(ctx, UploadTimeout)
		defer cancel()

		// A failed transfer closes its session; cleanup then needs a new one.
		reopen := func(ctx context.Context) (CommandExecutor, error) {
			return pool.GetOrOpen(ctx, devName)
		}
		if err := uploadFileWithReopen(uploadCtx, session, reopen, contentBytes, remotePath); err != nil {
			return fmt.Sprintf("Upload error: %v", err), true
		}

		sum := sha256.Sum256(contentBytes)
		sha := hex.EncodeToString(sum[:])
		return fmt.Sprintf("Successfully uploaded %d bytes to '%s' on %s (SHA-256: %s).",
			len(contentBytes), remotePath, devName, sha[:12]), false
	})

	// 4. ssh_download_file
	s.RegisterContextTool(map[string]any{
		"name":        "ssh_download_file",
		"description": "Read a remote file from the Raspberry Pi. Returns up to max_chars characters of text; a longer file is truncated, whatever its size. A binary file comes back as JSON with its size and a base64 prefix.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_name": map[string]any{
					"type":        "string",
					"description": "Name of the Raspberry Pi",
				},
				"remote_path": map[string]any{
					"type":        "string",
					"description": "File path to read (absolute, relative to the shell's directory, or starting with ~/)",
				},
				"max_chars": map[string]any{
					"type":        "integer",
					"description": "Maximum characters to return (default 15000)",
					"default":     15000,
				},
			},
			"required": []string{"device_name", "remote_path"},
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		devName, _ := args["device_name"].(string)
		remotePath, _ := args["remote_path"].(string)

		if strings.TrimSpace(devName) == "" {
			return "Validation error: device_name cannot be empty", true
		}
		if strings.TrimSpace(remotePath) == "" {
			return "Validation error: remote_path cannot be empty", true
		}

		maxChars, err := parseBoundedInt(args["max_chars"], DefaultDownloadChars, 1, 1000000, "max_chars")
		if err != nil {
			return err.Error(), true
		}

		if pool == nil {
			return "WebRTC session pool is not initialized", true
		}

		if ctx.Err() != nil {
			return "Download cancelled", true
		}

		session, err := pool.GetOrOpen(ctx, devName)
		if err != nil {
			return fmt.Sprintf("Connection error: %v", err), true
		}

		downloadCtx, cancel := context.WithTimeout(ctx, DownloadTimeout)
		defer cancel()

		result, err := downloadFile(downloadCtx, session, remotePath, maxChars)
		if err != nil {
			return fmt.Sprintf("Download error: %v", err), true
		}
		return result, false
	})

	// 5. ssh_disconnect
	s.RegisterContextTool(map[string]any{
		"name":        "ssh_disconnect",
		"description": "Close persistent Remote Shell session for a Raspberry Pi.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_name": map[string]any{
					"type":        "string",
					"description": "Name of the Raspberry Pi",
				},
			},
			"required": []string{"device_name"},
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		devName, _ := args["device_name"].(string)
		if strings.TrimSpace(devName) == "" {
			return "Validation error: device_name cannot be empty", true
		}

		if pool == nil {
			return "WebRTC session pool is not initialized", true
		}

		msg, err := pool.Disconnect(ctx, devName)
		if err != nil {
			return fmt.Sprintf("Error disconnecting: %v", err), true
		}
		return msg, false
	})
}

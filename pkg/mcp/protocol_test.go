package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveLines feeds complete request lines to a server and returns the
// response lines it wrote.
func serveLines(t *testing.T, server *Server, lines ...string) []string {
	t.Helper()
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	var responses []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) != "" {
			responses = append(responses, line)
		}
	}
	return responses
}

func TestPingIsAnswered(t *testing.T) {
	responses := serveLines(t, NewServer(), `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if len(responses) != 1 {
		t.Fatalf("expected one response, got %q", responses)
	}
	var resp struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *JSONRPCError   `json:"error"`
	}
	if err := json.Unmarshal([]byte(responses[0]), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("ping was rejected: %+v", resp.Error)
	}
	if resp.ID != 7 || string(resp.Result) != "{}" {
		t.Fatalf("expected an empty result for id 7, got %s", responses[0])
	}
}

// One oversized message must not take the whole server down: it is rejected
// and the requests after it are still served.
func TestOversizedRequestIsRejectedAndServerKeepsRunning(t *testing.T) {
	server := NewServer()
	server.SetMaxRequestBytes(1024)

	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", 200*1024) + `"}}`
	responses := serveLines(t, server, huge, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(responses) != 2 {
		t.Fatalf("expected a rejection and a ping reply, got %d responses: %.300q", len(responses), responses)
	}

	var rejected struct {
		Error *JSONRPCError `json:"error"`
	}
	if err := json.Unmarshal([]byte(responses[0]), &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Error == nil || rejected.Error.Code != CodeInvalidRequest || !strings.Contains(rejected.Error.Message, "too large") {
		t.Fatalf("expected an invalid-request error about size, got %s", responses[0])
	}
	if !strings.Contains(responses[1], `"id":2`) || !strings.Contains(responses[1], `"result"`) {
		t.Fatalf("request after the oversized one was not served: %s", responses[1])
	}
}

func TestRequestAtTheSizeLimitIsAccepted(t *testing.T) {
	server := NewServer()
	line := `{"jsonrpc":"2.0","id":3,"method":"ping"}`
	server.SetMaxRequestBytes(len(line))
	responses := serveLines(t, server, line)
	if len(responses) != 1 || !strings.Contains(responses[0], `"result"`) {
		t.Fatalf("request of exactly the limit was not served: %q", responses)
	}
}

func TestLastRequestWithoutNewlineIsServed(t *testing.T) {
	var out bytes.Buffer
	if err := NewServer().Serve(strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"ping"}`), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":4`) {
		t.Fatalf("final line without newline was dropped: %q", out.String())
	}
}

// The result text is read by a model, so "<" and "&" must arrive as typed,
// not as JSON unicode escapes.
func TestExecOutputIsNotHTMLEscaped(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, newBashPool(t, t.TempDir(), 0))

	text, isError := callTool(t, server, "ssh_exec", map[string]any{
		"device_name": "pi",
		"command":     `echo '<a href=x>1 & 2</a>'`,
	})
	if isError {
		t.Fatalf("ssh_exec failed: %s", text)
	}
	// The raw text must carry the characters themselves, not JSON escapes.
	if !strings.Contains(text, "<a href=x>1 & 2</a>") {
		t.Fatalf("output is HTML-escaped: %s", text)
	}
	if res := parseExecResult(t, text); res.Stdout != "<a href=x>1 & 2</a>" {
		t.Fatalf("unexpected stdout %q", res.Stdout)
	}
}

func TestExecReportsWhetherOutputWasTruncated(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, newBashPool(t, t.TempDir(), 0))

	text, isError := callTool(t, server, "ssh_exec", map[string]any{
		"device_name":      "pi",
		"command":          "seq 1 5000",
		"max_output_chars": 200,
	})
	if isError {
		t.Fatalf("ssh_exec failed: %s", text)
	}
	if !strings.Contains(text, `"truncated": true`) {
		t.Fatalf("truncated output is not flagged: %s", text)
	}

	text, isError = callTool(t, server, "ssh_exec", map[string]any{
		"device_name": "pi",
		"command":     "echo short",
	})
	if isError {
		t.Fatalf("ssh_exec failed: %s", text)
	}
	if !strings.Contains(text, `"truncated": false`) {
		t.Fatalf("complete output should be flagged as not truncated: %s", text)
	}
}

func TestUploadBinaryContentAsBase64(t *testing.T) {
	dir := t.TempDir()
	server := NewServer()
	RegisterRPiTools(server, nil, newBashPool(t, dir, 0))

	binary := []byte{0x00, 0xFF, 0xFE, 0x80, 0x0A, 0x1B, 0x7F, 0xC0, 0x00, 0x01}
	text, isError := callTool(t, server, "ssh_upload_file", map[string]any{
		"device_name":    "pi",
		"remote_path":    "image.bin",
		"content_base64": base64.StdEncoding.EncodeToString(binary),
	})
	if isError {
		t.Fatalf("binary upload failed: %s", text)
	}
	got, err := os.ReadFile(filepath.Join(dir, "image.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("uploaded bytes differ: got %x, want %x", got, binary)
	}
}

func TestUploadContentArgumentValidation(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, newBashPool(t, t.TempDir(), 0))

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"neither", map[string]any{}, "content or content_base64"},
		{"both", map[string]any{"content": "a", "content_base64": "YQ=="}, "only one of"},
		{"invalid base64", map[string]any{"content_base64": "not base64!"}, "not valid base64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"device_name": "pi", "remote_path": "x.bin"}
			for k, v := range tc.args {
				args[k] = v
			}
			text, isError := callTool(t, server, "ssh_upload_file", args)
			if !isError || !strings.Contains(text, tc.want) {
				t.Fatalf("expected a validation error containing %q, got isError=%v: %s", tc.want, isError, text)
			}
		})
	}
}

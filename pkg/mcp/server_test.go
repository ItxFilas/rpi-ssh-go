package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rpi-ssh-go/pkg/connect"
	"rpi-ssh-go/pkg/rtc"
)

func TestInitialize(t *testing.T) {
	server := NewServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d: %q", len(lines), out.String())
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %q", resp.JSONRPC)
	}
	if string(resp.ID) != "1" {
		t.Errorf("expected id 1, got %s", string(resp.ID))
	}
	if resp.Error != nil {
		t.Errorf("unexpected error in response: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not map[string]any: %v", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %v", result["protocolVersion"])
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok || serverInfo["name"] != "rpi-connect-ssh-go" {
		t.Errorf("expected serverInfo name rpi-connect-ssh-go, got %v", serverInfo)
	}
}

func TestToolsList(t *testing.T) {
	server := NewServer()
	server.RegisterTool(map[string]any{
		"name":        "dummy_tool",
		"description": "A dummy tool for testing",
		"inputSchema": map[string]any{
			"type": "object",
		},
	}, func(args map[string]any) (string, bool) {
		return "ok", false
	})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	resMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map, got %T", resp.Result)
	}
	tools, ok := resMap["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool in tools list, got %v", tools)
	}
}

func TestUnknownMethodReturns32601(t *testing.T) {
	server := NewServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":100,"method":"nonexistent/method"}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected 1 error response line, got: %q", out.String())
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object in response, got %v", resp)
	}

	code, _ := errObj["code"].(float64)
	if int(code) != -32601 {
		t.Errorf("expected error code -32601 (Method not found), got %v", code)
	}
}

func TestInvalidJSONSyntaxReturns32700(t *testing.T) {
	server := NewServer()
	in := strings.NewReader(`{ invalid json syntax ]` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected 1 error response line for parse error, got: %q", out.String())
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got: %v", resp)
	}

	code, _ := errObj["code"].(float64)
	if int(code) != -32700 {
		t.Errorf("expected error code -32700 (Parse error), got %v", code)
	}

	if resp["id"] != nil {
		t.Errorf("expected id null for parse error, got %v", resp["id"])
	}
}

func TestInvalidRequestMissingMethodReturns32600(t *testing.T) {
	server := NewServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":4}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected 1 error response line, got: %q", out.String())
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object: %v", resp)
	}
	code, _ := errObj["code"].(float64)
	if int(code) != -32600 {
		t.Errorf("expected error code -32600 (Invalid Request), got %v", code)
	}
}

func TestToolsCallMissingRequiredParams(t *testing.T) {
	server := NewServer()
	server.RegisterTool(map[string]any{
		"name": "strict_tool",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"required_field": map[string]any{
					"type": "string",
				},
			},
			"required": []string{"required_field"},
		},
	}, func(args map[string]any) (string, bool) {
		return "should not be called", false
	})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"strict_tool","arguments":{}}}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected 1 response line, got: %q", out.String())
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	// Must return code -32602 OR tool error (isError: true)
	if errObj, ok := resp["error"].(map[string]any); ok {
		code, _ := errObj["code"].(float64)
		if int(code) != -32602 {
			t.Errorf("expected error code -32602, got %v", code)
		}
	} else if result, ok := resp["result"].(map[string]any); ok {
		isError, _ := result["isError"].(bool)
		if !isError {
			t.Errorf("expected isError: true when missing required params, got result: %v", result)
		}
	} else {
		t.Errorf("expected error or tool error response, got %v", resp)
	}
}

func TestNotificationsEmitZeroLines(t *testing.T) {
	server := NewServer()
	notifications := []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":999}}`,
		`{"jsonrpc":"2.0","method":"notifications/unknown_notification","params":{"key":"val"}}`,
		`{"jsonrpc":"2.0","method":"custom/event_without_id","params":{}}`,
	}

	for _, notif := range notifications {
		in := strings.NewReader(notif + "\n")
		var out bytes.Buffer

		err := server.Serve(in, &out)
		if err != nil {
			t.Fatalf("Serve failed on notification %s: %v", notif, err)
		}

		if out.Len() > 0 {
			t.Errorf("expected 0 output bytes for notification %s, got: %q", notif, out.String())
		}
	}
}

func TestCancellationSignalHandling(t *testing.T) {
	server := NewServer()

	toolStarted := make(chan struct{})
	toolCancelled := make(chan bool, 1)

	server.RegisterContextTool(map[string]any{
		"name": "cancellable_tool",
		"inputSchema": map[string]any{
			"type": "object",
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		close(toolStarted)
		select {
		case <-ctx.Done():
			toolCancelled <- true
			return "aborted", true
		case <-time.After(2 * time.Second):
			toolCancelled <- false
			return "completed", false
		}
	})

	rPipe, wPipe := io.Pipe()

	var out bytes.Buffer
	var serveErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = server.Serve(rPipe, &out)
	}()

	// 1. Send tools/call
	callPayload := `{"jsonrpc":"2.0","id":"req-cancel-1","method":"tools/call","params":{"name":"cancellable_tool","arguments":{}}}` + "\n"
	_, _ = wPipe.Write([]byte(callPayload))

	// Wait until tool starts executing
	select {
	case <-toolStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for tool to start")
	}

	// 2. Send notifications/cancelled
	cancelPayload := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"req-cancel-1"}}` + "\n"
	_, _ = wPipe.Write([]byte(cancelPayload))

	// 3. Wait to verify context was cancelled quickly (should not take 2s)
	select {
	case wasCancelled := <-toolCancelled:
		if !wasCancelled {
			t.Error("expected tool execution to be cancelled via context, but it completed normally")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("cancellation signal was not received by tool within 1 second")
	}

	_ = wPipe.Close()
	wg.Wait()

	if serveErr != nil {
		t.Fatalf("Serve exited with error: %v", serveErr)
	}

	// Per MCP specification:
	// A cancelled request should NOT send a regular response, and notifications emit 0 output lines.
	if strings.Contains(out.String(), `"id":"req-cancel-1"`) {
		t.Errorf("cancelled request should not emit response, but got: %q", out.String())
	}
}

func TestRPiToolsSchemaDefaults(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, nil)

	sMap := make(map[string]map[string]any)
	for _, def := range server.toolDefinitions {
		name, _ := def["name"].(string)
		sMap[name] = def
	}

	// Verify ssh_exec has required device_name and command, and defaults
	execDef, ok := sMap["ssh_exec"]
	if !ok {
		t.Fatal("ssh_exec tool not found")
	}
	execSchema, ok := execDef["inputSchema"].(map[string]any)
	if !ok {
		t.Fatal("ssh_exec inputSchema missing")
	}
	props, ok := execSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("ssh_exec properties missing")
	}

	timeoutProp, ok := props["timeout_seconds"].(map[string]any)
	if !ok || timeoutProp["default"] != 30 {
		t.Errorf("expected timeout_seconds default: 30, got: %v", timeoutProp["default"])
	}

	maxOutputProp, ok := props["max_output_chars"].(map[string]any)
	if !ok || maxOutputProp["default"] != 8000 {
		t.Errorf("expected max_output_chars default: 8000, got: %v", maxOutputProp["default"])
	}

	// Verify ssh_download_file default max_chars: 15000
	dlDef, ok := sMap["ssh_download_file"]
	if !ok {
		t.Fatal("ssh_download_file tool not found")
	}
	dlProps := dlDef["inputSchema"].(map[string]any)["properties"].(map[string]any)
	maxCharsProp := dlProps["max_chars"].(map[string]any)
	if maxCharsProp["default"] != 15000 {
		t.Errorf("expected max_chars default: 15000, got: %v", maxCharsProp["default"])
	}

	// Verify ssh_list_devices default force_refresh: false
	listDef, ok := sMap["ssh_list_devices"]
	if !ok {
		t.Fatal("ssh_list_devices tool not found")
	}
	listProps := listDef["inputSchema"].(map[string]any)["properties"].(map[string]any)
	frProp := listProps["force_refresh"].(map[string]any)
	if frProp["default"] != false {
		t.Errorf("expected force_refresh default: false, got: %v", frProp["default"])
	}
}

func TestSchemaTypeValidation(t *testing.T) {
	server := NewServer()
	server.RegisterTool(map[string]any{
		"name": "type_test_tool",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{
					"type": "integer",
				},
				"flag": map[string]any{
					"type": "boolean",
				},
			},
		},
	}, func(args map[string]any) (string, bool) {
		return "should not reach here", false
	})

	// 1. Send invalid integer (string provided instead of int)
	in1 := strings.NewReader(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"type_test_tool","arguments":{"count":"not-a-number"}}}` + "\n")
	var out1 bytes.Buffer
	if err := server.Serve(in1, &out1); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	var resp1 map[string]any
	json.Unmarshal(out1.Bytes(), &resp1)
	errObj1, _ := resp1["error"].(map[string]any)
	if errObj1 == nil || int(errObj1["code"].(float64)) != -32602 {
		t.Errorf("expected -32602 error for invalid integer type, got: %v", resp1)
	}

	// 2. Send invalid boolean (number provided instead of bool)
	in2 := strings.NewReader(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"type_test_tool","arguments":{"flag":123}}}` + "\n")
	var out2 bytes.Buffer
	if err := server.Serve(in2, &out2); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	var resp2 map[string]any
	json.Unmarshal(out2.Bytes(), &resp2)
	errObj2, _ := resp2["error"].(map[string]any)
	if errObj2 == nil || int(errObj2["code"].(float64)) != -32602 {
		t.Errorf("expected -32602 error for invalid boolean type, got: %v", resp2)
	}
}

func TestLargePayloadHandling(t *testing.T) {
	server := NewServer()
	receivedSize := 0
	server.RegisterTool(map[string]any{
		"name": "large_payload_tool",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data": map[string]any{"type": "string"},
			},
		},
	}, func(args map[string]any) (string, bool) {
		str, _ := args["data"].(string)
		receivedSize = len(str)
		return "ok", false
	})

	// Create payload > 64KB (e.g. 100KB)
	largeData := strings.Repeat("A", 100*1024)
	reqJSON, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "large_payload_tool",
			"arguments": map[string]any{
				"data": largeData,
			},
		},
	})

	in := strings.NewReader(string(reqJSON) + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve failed on large payload: %v", err)
	}

	if receivedSize != 100*1024 {
		t.Errorf("expected 102400 bytes, got %d", receivedSize)
	}
}

func TestCancellationByTypedID(t *testing.T) {
	server := NewServer()
	started := make(chan struct{})
	cancelled := make(chan bool, 1)

	server.RegisterContextTool(map[string]any{
		"name": "num_cancel_tool",
		"inputSchema": map[string]any{
			"type": "object",
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		close(started)
		select {
		case <-ctx.Done():
			cancelled <- true
			return "aborted", true
		case <-time.After(2 * time.Second):
			cancelled <- false
			return "done", false
		}
	})

	rPipe, wPipe := io.Pipe()
	var out bytes.Buffer
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = server.Serve(rPipe, &out)
	}()

	// Send request with integer ID 500
	_, _ = wPipe.Write([]byte(`{"jsonrpc":"2.0","id":500,"method":"tools/call","params":{"name":"num_cancel_tool","arguments":{}}}` + "\n"))

	select {
	case <-started:
	case <-time.After(1 * time.Second):
		t.Fatal("tool did not start")
	}

	// Send cancel notification with string ID "500" - MUST NOT cancel numeric 500
	_, _ = wPipe.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"500"}}` + "\n"))

	// Give a short window to confirm context was NOT cancelled
	select {
	case <-cancelled:
		t.Fatal("cancelling string '500' incorrectly cancelled integer ID 500")
	case <-time.After(100 * time.Millisecond):
		// Expected: tool is still running
	}

	// Now send cancel notification with numeric ID 500 - MUST cancel
	_, _ = wPipe.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":500}}` + "\n"))

	select {
	case wasCancelled := <-cancelled:
		if !wasCancelled {
			t.Error("expected tool with int id 500 to be cancelled by numeric 500")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for cancel with numeric ID")
	}

	_ = wPipe.Close()
	wg.Wait()
}

type threadSafeBuffer struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (t *threadSafeBuffer) Write(p []byte) (n int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func TestNinthConcurrentOperationRejected(t *testing.T) {
	server := NewServer()

	startedCount := make(chan struct{}, 8)
	release := make(chan struct{})

	server.RegisterContextTool(map[string]any{
		"name": "slow_op",
		"inputSchema": map[string]any{
			"type": "object",
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		startedCount <- struct{}{}
		select {
		case <-release:
			return "ok", false
		case <-ctx.Done():
			return "aborted", true
		}
	})

	rPipe, wPipe := io.Pipe()
	var outMu sync.Mutex
	var out bytes.Buffer

	safeOut := &threadSafeBuffer{b: &out, mu: &outMu}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = server.Serve(rPipe, safeOut)
	}()

	// Launch 8 concurrent slow operations
	for i := 1; i <= 8; i++ {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"slow_op","arguments":{}}}`+"\n", i)
		_, _ = wPipe.Write([]byte(req))
	}

	// Wait until all 8 are actively running
	for i := 1; i <= 8; i++ {
		select {
		case <-startedCount:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for active operation %d to start", i)
		}
	}

	// Now send 9th operation
	req9 := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"slow_op","arguments":{}}}` + "\n"
	_, _ = wPipe.Write([]byte(req9))

	// Wait briefly for 9th response
	var ninthRespLine string
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		outMu.Unlock()
		for _, line := range lines {
			if strings.Contains(line, `"id":9`) {
				ninthRespLine = line
				break
			}
		}
		if ninthRespLine != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if ninthRespLine == "" {
		t.Fatalf("did not receive response for 9th operation, out: %q", out.String())
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal([]byte(ninthRespLine), &resp); err != nil {
		t.Fatalf("invalid json in 9th response: %v, raw: %s", err, ninthRespLine)
	}

	if resp.Error == nil {
		t.Fatalf("expected error for 9th operation, got success result: %v", resp.Result)
	}

	if resp.Error.Code != CodeServerBusy {
		t.Errorf("expected error code %d (CodeServerBusy), got %d", CodeServerBusy, resp.Error.Code)
	}

	if !strings.Contains(strings.ToLower(resp.Error.Message), "server busy") {
		t.Errorf("expected message to mention 'server busy', got: %q", resp.Error.Message)
	}

	// Release the 8 active operations and clean up
	close(release)
	_ = wPipe.Close()
	wg.Wait()
}

func TestLargeNumericIDRoundTrip(t *testing.T) {
	server := NewServer()
	// Large 64-bit integer ID that loses precision in IEEE-754 float64 (2^53 + 1 = 9007199254740993)
	largeID := "9007199254740993"
	in := strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"initialize"}`+"\n", largeID))
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	outStr := strings.TrimSpace(out.String())
	expectedIDPattern := fmt.Sprintf(`"id":%s`, largeID)
	expectedIDPatternSpaced := fmt.Sprintf(`"id": %s`, largeID)
	if !strings.Contains(outStr, expectedIDPattern) && !strings.Contains(outStr, expectedIDPatternSpaced) {
		t.Errorf("expected output to contain exact id %s, got: %s", largeID, outStr)
	}

	d := json.NewDecoder(strings.NewReader(outStr))
	d.UseNumber()
	var decoded map[string]any
	if err := d.Decode(&decoded); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	num, ok := decoded["id"].(json.Number)
	if !ok || num.String() != largeID {
		t.Errorf("expected decoded id to be json.Number(%s), got %v (%T)", largeID, decoded["id"], decoded["id"])
	}
}

func TestEOFStopsActiveWork(t *testing.T) {
	server := NewServer()

	toolStarted := make(chan struct{})
	toolContextCancelled := make(chan struct{})

	server.RegisterContextTool(map[string]any{
		"name": "hanging_tool",
		"inputSchema": map[string]any{
			"type": "object",
		},
	}, func(ctx context.Context, args map[string]any) (string, bool) {
		close(toolStarted)
		<-ctx.Done()
		close(toolContextCancelled)
		return "done", false
	})

	rPipe, wPipe := io.Pipe()
	var out bytes.Buffer
	serveDone := make(chan error, 1)

	go func() {
		serveDone <- server.Serve(rPipe, &out)
	}()

	// Send tool call
	_, _ = wPipe.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hanging_tool","arguments":{}}}` + "\n"))

	select {
	case <-toolStarted:
	case <-time.After(1 * time.Second):
		t.Fatal("tool did not start")
	}

	// Simulate EOF on stdin
	_ = wPipe.Close()

	// Verify tool context was cancelled because of EOF
	select {
	case <-toolContextCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool context was not cancelled upon stdin EOF")
	}

	// Verify Serve returns cleanly
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("expected Serve to return nil on clean EOF, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after stdin EOF and active work completion")
	}
}

func TestEmptyStringRequestID(t *testing.T) {
	server := NewServer()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":"","method":"initialize"}` + "\n")
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("expected exactly 1 response line for empty string ID (not notification), got %d lines: %q", len(lines), out.String())
	}

	var rawResp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &rawResp); err != nil {
		t.Fatalf("failed to parse JSON response: %v", err)
	}

	idRaw, hasID := rawResp["id"]
	if !hasID {
		t.Fatal("response is missing id field")
	}
	if string(idRaw) != `""` {
		t.Errorf("expected response id to be `\"\"`, got %s", string(idRaw))
	}
}

func TestNumericArgumentBounds(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, nil) // nil client & pool

	callTool := func(toolName string, args map[string]any) (isErr bool, msg string) {
		reqJSON, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": args,
			},
		})
		in := strings.NewReader(string(reqJSON) + "\n")
		var out bytes.Buffer
		_ = server.Serve(in, &out)
		var resp JSONRPCResponse
		_ = json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp)
		if resp.Error != nil {
			return true, resp.Error.Message
		}
		if resMap, ok := resp.Result.(map[string]any); ok {
			isE, _ := resMap["isError"].(bool)
			contentList, _ := resMap["content"].([]any)
			if len(contentList) > 0 {
				if cMap, ok := contentList[0].(map[string]any); ok {
					txt, _ := cMap["text"].(string)
					return isE, txt
				}
			}
			return isE, ""
		}
		return false, ""
	}

	// 1. ssh_exec: timeout_seconds bounds [1..300]
	_, msg := callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "timeout_seconds": 0})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected timeout_seconds: 0 to return Validation error, got: %q", msg)
	}
	_, msg = callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "timeout_seconds": -5})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected timeout_seconds: -5 to return Validation error, got: %q", msg)
	}
	_, msg = callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "timeout_seconds": 301})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected timeout_seconds: 301 to return Validation error, got: %q", msg)
	}

	// 2. ssh_exec: max_output_chars bounds [1..1,000,000]
	_, msg = callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "max_output_chars": 0})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected max_output_chars: 0 to return Validation error, got: %q", msg)
	}
	_, msg = callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "max_output_chars": 1000001})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected max_output_chars: 1000001 to return Validation error, got: %q", msg)
	}

	// 3. ssh_download_file: max_chars bounds [1..1,000,000]
	_, msg = callTool("ssh_download_file", map[string]any{"device_name": "pi", "remote_path": "/file", "max_chars": 0})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected max_chars: 0 to return Validation error, got: %q", msg)
	}
	_, msg = callTool("ssh_download_file", map[string]any{"device_name": "pi", "remote_path": "/file", "max_chars": -1})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected max_chars: -1 to return Validation error, got: %q", msg)
	}
	_, msg = callTool("ssh_download_file", map[string]any{"device_name": "pi", "remote_path": "/file", "max_chars": 1000001})
	if !strings.Contains(msg, "Validation error") {
		t.Errorf("expected max_chars: 1000001 to return Validation error, got: %q", msg)
	}

	// 4. Valid defaults and boundary values should pass validation (and reach pool check)
	isErr, msg := callTool("ssh_exec", map[string]any{"device_name": "pi", "command": "ls", "timeout_seconds": 1, "max_output_chars": 1})
	if strings.Contains(msg, "Validation error") {
		t.Errorf("valid bounds returned unexpected validation error: %s", msg)
	}
	if !strings.Contains(msg, "WebRTC session pool is not initialized") {
		t.Errorf("expected tool to reach pool check, got: %q (isErr: %v)", msg, isErr)
	}

	isErr, msg = callTool("ssh_download_file", map[string]any{"device_name": "pi", "remote_path": "/file", "max_chars": 1000000})
	if strings.Contains(msg, "Validation error") {
		t.Errorf("valid max_chars 1000000 returned unexpected validation error: %s", msg)
	}
	if !strings.Contains(msg, "WebRTC session pool is not initialized") {
		t.Errorf("expected download tool to reach pool check, got: %q (isErr: %v)", msg, isErr)
	}
}

func TestConcurrentRequests(t *testing.T) {
	server := NewServer()
	server.RegisterTool(map[string]any{
		"name": "echo",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"val": map[string]any{"type": "string"},
			},
		},
	}, func(args map[string]any) (string, bool) {
		val, _ := args["val"].(string)
		return val, false
	})

	var inputBuf bytes.Buffer
	count := 20
	for i := 1; i <= count; i++ {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{"val":"msg-%d"}}}`+"\n", i, i)
		inputBuf.WriteString(req)
	}

	var out bytes.Buffer
	err := server.Serve(&inputBuf, &out)
	if err != nil {
		t.Fatalf("Serve error: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != count {
		t.Fatalf("expected %d response lines, got %d", count, len(lines))
	}

	receivedIDs := make(map[int]bool)
	for _, line := range lines {
		var resp JSONRPCResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("invalid json response line: %s", line)
		}
		idNum, _ := strconv.Atoi(string(resp.ID))
		receivedIDs[idNum] = true
	}

	for i := 1; i <= count; i++ {
		if !receivedIDs[i] {
			t.Errorf("missing response for request id %d", i)
		}
	}
}

type fakeStalledConnectClient struct{}

func (f *fakeStalledConnectClient) ListDevices(ctx context.Context, forceRefresh bool) ([]connect.DeviceInfo, error) {
	return []connect.DeviceInfo{
		{ID: "dev-slow-123", Name: "slow-pi", Online: true},
	}, nil
}

func (f *fakeStalledConnectClient) GetShellSessionInfo(ctx context.Context, deviceID string) (connect.ICEConfig, string, error) {
	select {
	case <-ctx.Done():
		return connect.ICEConfig{}, "", ctx.Err()
	case <-time.After(5 * time.Second):
		return connect.ICEConfig{}, "", fmt.Errorf("should have timed out before 5s")
	}
}

func (f *fakeStalledConnectClient) PostOffer(ctx context.Context, deviceID, sdp, csrfToken, referer string) (string, error) {
	return "conn-123", nil
}

func (f *fakeStalledConnectClient) PollAnswer(ctx context.Context, deviceID, connID, referer string, maxDuration time.Duration) (string, error) {
	return "", fmt.Errorf("not implemented")
}

// The command timeout does not cover connecting, so a connect that never
// completes has to be cut off by the pool's own setup timeout.
func TestStalledConnectIsBoundedBySetupTimeout(t *testing.T) {
	server := NewServer()
	pool := rtc.NewPool(nil)
	pool.SetClient(&fakeStalledConnectClient{})
	pool.SetSetupTimeout(time.Second)

	RegisterRPiTools(server, nil, pool)

	// The command timeout is long; only the setup timeout can end this call early.
	reqJSON := `{"jsonrpc":"2.0","id":101,"method":"tools/call","params":{"name":"ssh_exec","arguments":{"device_name":"slow-pi","command":"echo hello","timeout_seconds":30}}}` + "\n"
	rPipe, wPipe := io.Pipe()
	var outMu sync.Mutex
	var out bytes.Buffer
	safeOut := &threadSafeBuffer{b: &out, mu: &outMu}

	start := time.Now()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(rPipe, safeOut)
	}()

	_, _ = wPipe.Write([]byte(reqJSON))

	// Wait for response to be written before closing stdin
	for {
		outMu.Lock()
		hasResp := strings.Contains(out.String(), `"id":101`)
		outMu.Unlock()
		if hasResp || time.Since(start) > 4*time.Second {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = wPipe.Close()
	err := <-serveDone
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	// The call should finish close to 1 second, well before the 5-second connector stall
	if elapsed > 3*time.Second {
		t.Errorf("expected operation to time out within ~1-2 seconds, but took %v", elapsed)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v, raw output: %q", err, out.String())
	}

	if string(resp.ID) != "101" {
		t.Errorf("expected response ID 101, got %s", string(resp.ID))
	}

	resMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result map in response, got: %v", resp)
	}

	isError, _ := resMap["isError"].(bool)
	if !isError {
		t.Errorf("expected isError to be true for stalled connection timeout, got false")
	}

	contentList, _ := resMap["content"].([]any)
	if len(contentList) == 0 {
		t.Fatalf("expected non-empty content in result")
	}
	firstContent, _ := contentList[0].(map[string]any)
	text, _ := firstContent["text"].(string)

	if !strings.Contains(strings.ToLower(text), "error") && !strings.Contains(strings.ToLower(text), "deadline") && !strings.Contains(strings.ToLower(text), "timeout") {
		t.Errorf("expected error message to mention error or timeout, got: %q", text)
	}
}

func TestUploadFileToolValidationAndEmptyContent(t *testing.T) {
	server := NewServer()
	RegisterRPiTools(server, nil, nil) // pool is nil

	// 1. Content = "" (empty string) must pass schema validation and reach handler
	// (handler returns pool is not initialized, rather than schema rejection)
	reqJSON := `{"jsonrpc":"2.0","id":201,"method":"tools/call","params":{"name":"ssh_upload_file","arguments":{"device_name":"my-pi","remote_path":"/tmp/empty.txt","content":""}}}` + "\n"
	in := strings.NewReader(reqJSON)
	var out bytes.Buffer

	err := server.Serve(in, &out)
	if err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	var resp JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	if resp.Error != nil {
		t.Fatalf("Expected no schema error for content=\"\", got RPC error: %v", resp.Error)
	}

	resMap, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("Expected result map, got: %v", resp.Result)
	}
	contentList, _ := resMap["content"].([]any)
	firstContent, _ := contentList[0].(map[string]any)
	text, _ := firstContent["text"].(string)

	if !strings.Contains(text, "WebRTC session pool is not initialized") {
		t.Errorf("Expected handler execution (pool uninitialized), got: %q", text)
	}

	// 2. Empty device_name must be rejected by handler
	reqEmptyDev := `{"jsonrpc":"2.0","id":202,"method":"tools/call","params":{"name":"ssh_upload_file","arguments":{"device_name":"  ","remote_path":"/tmp/file.txt","content":"data"}}}` + "\n"
	out.Reset()
	if err := server.Serve(strings.NewReader(reqEmptyDev), &out); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	var respDev JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &respDev); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	resDevMap, _ := respDev.Result.(map[string]any)
	cDevList, _ := resDevMap["content"].([]any)
	fDevContent, _ := cDevList[0].(map[string]any)
	tDevText, _ := fDevContent["text"].(string)
	if !strings.Contains(tDevText, "device_name cannot be empty") {
		t.Errorf("Expected device_name empty validation error, got: %q", tDevText)
	}

	// 3. Empty remote_path must be rejected by handler
	reqEmptyPath := `{"jsonrpc":"2.0","id":203,"method":"tools/call","params":{"name":"ssh_upload_file","arguments":{"device_name":"my-pi","remote_path":"","content":"data"}}}` + "\n"
	out.Reset()
	if err := server.Serve(strings.NewReader(reqEmptyPath), &out); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	var respPath JSONRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &respPath); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	resPathMap, _ := respPath.Result.(map[string]any)
	cPathList, _ := resPathMap["content"].([]any)
	fPathContent, _ := cPathList[0].(map[string]any)
	tPathText, _ := fPathContent["text"].(string)
	if !strings.Contains(tPathText, "remote_path cannot be empty") {
		t.Errorf("Expected remote_path empty validation error, got: %q", tPathText)
	}
}

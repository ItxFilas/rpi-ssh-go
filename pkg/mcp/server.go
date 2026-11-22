package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// Standard JSON-RPC 2.0 error codes
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeServerBusy     = -32000
)

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type ToolHandler func(args map[string]any) (resultText string, isError bool)
type ContextToolHandler func(ctx context.Context, args map[string]any) (resultText string, isError bool)

type Server struct {
	toolDefinitions []map[string]any
	toolHandlers    map[string]ContextToolHandler
	toolSchemas     map[string]map[string]any
	mu              sync.RWMutex

	activeRequests map[string]context.CancelFunc
	userCancelled  map[string]bool
	activeReqMu    sync.Mutex
	writeMu        sync.Mutex
	wg             sync.WaitGroup
	sem            chan struct{}

	maxRequestBytes int
}

// DefaultMaxRequestBytes is the largest request line the server accepts. It
// leaves room for an 8 MiB upload sent as base64 inside JSON.
const DefaultMaxRequestBytes = 32 * 1024 * 1024

// SetMaxRequestBytes changes the largest request line the server accepts.
func (s *Server) SetMaxRequestBytes(n int) {
	if n > 0 {
		s.maxRequestBytes = n
	}
}

func NewServer() *Server {
	return &Server{
		toolDefinitions: make([]map[string]any, 0),
		toolHandlers:    make(map[string]ContextToolHandler),
		toolSchemas:     make(map[string]map[string]any),
		activeRequests:  make(map[string]context.CancelFunc),
		userCancelled:   make(map[string]bool),
		sem:             make(chan struct{}, 8),
		maxRequestBytes: DefaultMaxRequestBytes,
	}
}

// RegisterTool registers a tool handler without context.
func (s *Server) RegisterTool(definition map[string]any, handler ToolHandler) {
	s.RegisterContextTool(definition, func(ctx context.Context, args map[string]any) (string, bool) {
		return handler(args)
	})
}

// RegisterContextTool registers a context-aware tool handler that respects request cancellation.
func (s *Server) RegisterContextTool(definition map[string]any, handler ContextToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name, _ := definition["name"].(string)
	s.toolDefinitions = append(s.toolDefinitions, definition)
	s.toolHandlers[name] = handler
	if schema, ok := definition["inputSchema"].(map[string]any); ok {
		s.toolSchemas[name] = schema
	}
}

// ToolDefinitions returns a copy of all registered tool definitions.
func (s *Server) ToolDefinitions() []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defs := make([]map[string]any, len(s.toolDefinitions))
	copy(defs, s.toolDefinitions)
	return defs
}

func typedIDKey(id any) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case json.RawMessage:
		raw := bytes.TrimSpace(v)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return ""
		}
		if raw[0] == '"' {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				return "str:" + s
			}
			return "str:" + string(raw)
		}
		return "num:" + string(raw)
	case string:
		return "str:" + v
	case int:
		return "num:" + strconv.Itoa(v)
	case int64:
		return "num:" + strconv.FormatInt(v, 10)
	case int32:
		return "num:" + strconv.FormatInt(int64(v), 10)
	case uint:
		return "num:" + strconv.FormatUint(uint64(v), 10)
	case uint64:
		return "num:" + strconv.FormatUint(v, 10)
	case json.Number:
		return "num:" + v.String()
	case float64:
		if v == float64(int64(v)) {
			return "num:" + strconv.FormatInt(int64(v), 10)
		}
		return "num:" + strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%T:%v", id, id)
	}
}

func normalizeID(id any) string {
	return typedIDKey(id)
}

func (s *Server) registerCancel(id any, cancel context.CancelFunc) {
	key := typedIDKey(id)
	if key == "" {
		return
	}
	s.activeReqMu.Lock()
	s.activeRequests[key] = cancel
	s.activeReqMu.Unlock()
}

func (s *Server) unregisterCancel(id any) {
	key := typedIDKey(id)
	if key == "" {
		return
	}
	s.activeReqMu.Lock()
	delete(s.activeRequests, key)
	delete(s.userCancelled, key)
	s.activeReqMu.Unlock()
}

// CancelRequest aborts an active request matching the given ID.
func (s *Server) CancelRequest(id any) bool {
	key := typedIDKey(id)
	if key == "" {
		return false
	}
	s.activeReqMu.Lock()
	cancel, ok := s.activeRequests[key]
	if ok {
		s.userCancelled[key] = true
	}
	s.activeReqMu.Unlock()

	if ok && cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Server) isCancelled(id any) bool {
	key := typedIDKey(id)
	if key == "" {
		return false
	}
	s.activeReqMu.Lock()
	defer s.activeReqMu.Unlock()
	return s.userCancelled[key]
}

// ActiveRequestCount returns the number of currently running requests.
func (s *Server) ActiveRequestCount() int {
	s.activeReqMu.Lock()
	defer s.activeReqMu.Unlock()
	return len(s.activeRequests)
}

func (s *Server) sendResponse(out io.Writer, resp JSONRPCResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = out.Write(data)
}

func (s *Server) sendError(out io.Writer, id json.RawMessage, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
	s.sendResponse(out, resp)
}

func validateArguments(schema map[string]any, args map[string]any) error {
	if schema == nil {
		return nil
	}

	// 1. Required fields
	if reqVal, ok := schema["required"]; ok {
		switch reqList := reqVal.(type) {
		case []string:
			for _, reqKey := range reqList {
				val, exists := args[reqKey]
				if !exists || val == nil {
					return fmt.Errorf("missing required parameter '%s'", reqKey)
				}
			}
		case []any:
			for _, r := range reqList {
				if reqKey, ok := r.(string); ok {
					val, exists := args[reqKey]
					if !exists || val == nil {
						return fmt.Errorf("missing required parameter '%s'", reqKey)
					}
				}
			}
		}
	}

	// 2. Type validation
	if propsVal, ok := schema["properties"].(map[string]any); ok {
		for argName, argVal := range args {
			if propDef, ok := propsVal[argName].(map[string]any); ok {
				expectedType, _ := propDef["type"].(string)
				switch expectedType {
				case "string":
					if _, ok := argVal.(string); !ok {
						return fmt.Errorf("parameter '%s' must be a string", argName)
					}
				case "boolean":
					if _, ok := argVal.(bool); !ok {
						return fmt.Errorf("parameter '%s' must be a boolean", argName)
					}
				case "integer":
					switch v := argVal.(type) {
					case float64:
						if v != float64(int64(v)) {
							return fmt.Errorf("parameter '%s' must be an integer", argName)
						}
					case int, int64, int32:
					case json.Number:
						if _, err := v.Int64(); err != nil {
							return fmt.Errorf("parameter '%s' must be an integer", argName)
						}
					default:
						return fmt.Errorf("parameter '%s' must be an integer", argName)
					}
				case "number":
					switch v := argVal.(type) {
					case float64, float32, int, int64, int32:
					case json.Number:
						if _, err := v.Float64(); err != nil {
							return fmt.Errorf("parameter '%s' must be a number", argName)
						}
					default:
						return fmt.Errorf("parameter '%s' must be a number", argName)
					}
				}
			}
		}
	}

	return nil
}

func (s *Server) Serve(in io.Reader, out io.Writer) error {
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	reader := bufio.NewReaderSize(in, 64*1024)
	var lineBuf []byte
	var readErr error

	for readErr == nil {
		if cap(lineBuf) > 1024*1024 {
			// Do not hold on to the buffer of an unusually large request.
			lineBuf = nil
		}
		var tooLong bool
		lineBuf, tooLong, readErr = readRequestLine(reader, lineBuf[:0], s.maxRequestBytes)
		if readErr != nil && readErr != io.EOF {
			break
		}
		if tooLong {
			// The id is somewhere in the discarded line, so the reply
			// cannot name it. The server keeps serving later requests.
			s.sendError(out, nil, CodeInvalidRequest, fmt.Sprintf("Invalid Request: message too large (limit %d bytes)", s.maxRequestBytes))
			continue
		}

		// The line is parsed straight from the read buffer, once. Decoding
		// copies everything it keeps, so nothing aliases the buffer afterwards.
		line := bytes.TrimSpace(lineBuf)
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// Parse error: Invalid JSON was received by the server
			s.sendError(out, nil, CodeParseError, fmt.Sprintf("Parse error: %v", err))
			continue
		}

		// A missing or null id leaves req.ID empty or "null".
		trimmedID := bytes.TrimSpace(req.ID)
		isNotification := len(trimmedID) == 0 || bytes.Equal(trimmedID, []byte("null")) || strings.HasPrefix(req.Method, "notifications/") || req.Method == "$/cancelRequest"

		// Check JSON-RPC version
		if req.JSONRPC != "2.0" {
			if !isNotification {
				s.sendError(out, req.ID, CodeInvalidRequest, "Invalid Request: jsonrpc must be '2.0'")
			}
			continue
		}

		// Check method presence
		if req.Method == "" {
			if !isNotification {
				s.sendError(out, req.ID, CodeInvalidRequest, "Invalid Request: method is required")
			}
			continue
		}

		// Handle notifications (MUST emit 0 output lines)
		if isNotification {
			s.handleNotification(req)
			continue
		}

		// Handle requests with ID
		s.handleRequest(rootCtx, out, req)
	}

	cancelRoot()
	s.wg.Wait()
	if readErr == io.EOF {
		return nil
	}
	return readErr
}

// readRequestLine reads one newline-terminated line into buf and returns it
// without the newline. A line longer than maxBytes is consumed to its end and
// reported as tooLong instead of being kept. At end of input it returns the
// final unterminated line, if any, together with io.EOF.
func readRequestLine(r *bufio.Reader, buf []byte, maxBytes int) (line []byte, tooLong bool, err error) {
	const lineEnd = 2 // room for the "\r\n" that is trimmed afterwards
	for {
		chunk, readErr := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxBytes+lineEnd {
				tooLong = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if tooLong {
			return buf[:0], true, readErr
		}
		line = bytes.TrimRight(buf, "\r\n")
		if len(line) > maxBytes {
			return buf[:0], true, readErr
		}
		return line, false, readErr
	}
}

func (s *Server) handleNotification(req JSONRPCRequest) {
	switch req.Method {
	case "notifications/cancelled", "$/cancelRequest":
		if len(req.Params) > 0 {
			var cancelParams struct {
				RequestID json.RawMessage `json:"requestId"`
				ID        json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(req.Params, &cancelParams)
			targetID := cancelParams.RequestID
			if len(targetID) == 0 || bytes.Equal(bytes.TrimSpace(targetID), []byte("null")) {
				targetID = cancelParams.ID
			}
			if len(targetID) > 0 && !bytes.Equal(bytes.TrimSpace(targetID), []byte("null")) {
				s.CancelRequest(targetID)
			}
		}
	case "notifications/initialized":
		// Initialization confirmed; no response needed
	default:
		// Unknown notifications are safely ignored per JSON-RPC / MCP specifications
	}
}

func (s *Server) handleRequest(ctx context.Context, out io.Writer, req JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]any{
					"name":    "rpi-connect-ssh-go",
					"version": "2.1.0",
				},
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": false,
					},
				},
			},
		}
		s.sendResponse(out, resp)

	case "ping":
		s.sendResponse(out, JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})

	case "tools/list":
		s.mu.RLock()
		defs := make([]map[string]any, len(s.toolDefinitions))
		copy(defs, s.toolDefinitions)
		s.mu.RUnlock()

		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": defs,
			},
		}
		s.sendResponse(out, resp)

	case "tools/call":
		if len(req.Params) == 0 {
			s.sendError(out, req.ID, CodeInvalidParams, "Invalid params: params object is required")
			return
		}

		var callParams struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			s.sendError(out, req.ID, CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err))
			return
		}
		if callParams.Name == "" {
			s.sendError(out, req.ID, CodeInvalidParams, "Invalid params: 'name' is required")
			return
		}
		if callParams.Arguments == nil {
			callParams.Arguments = make(map[string]any)
		}

		s.mu.RLock()
		handler, exists := s.toolHandlers[callParams.Name]
		schema := s.toolSchemas[callParams.Name]
		s.mu.RUnlock()

		if !exists {
			s.sendResponse(out, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{
						{
							"type": "text",
							"text": fmt.Sprintf("Unknown tool '%s'", callParams.Name),
						},
					},
					"isError": true,
				},
			})
			return
		}

		if err := validateArguments(schema, callParams.Arguments); err != nil {
			s.sendError(out, req.ID, CodeInvalidParams, fmt.Sprintf("Invalid params: %v", err))
			return
		}

		// Bounded concurrency admission: at most 8 concurrent active operations
		select {
		case s.sem <- struct{}{}:
		default:
			s.sendError(out, req.ID, CodeServerBusy, "server busy: maximum 8 concurrent operations")
			return
		}

		reqID := req.ID
		opCtx, cancel := context.WithCancel(ctx)
		s.registerCancel(reqID, cancel)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			defer cancel()
			defer s.unregisterCancel(reqID)

			resultText, isError := handler(opCtx, callParams.Arguments)

			// If the request was cancelled by user, do NOT send a regular response
			if s.isCancelled(reqID) {
				return
			}

			s.sendResponse(out, JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      reqID,
				Result: map[string]any{
					"content": []map[string]any{
						{
							"type": "text",
							"text": resultText,
						},
					},
					"isError": isError,
				},
			})
		}()

	default:
		s.sendError(out, req.ID, CodeMethodNotFound, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

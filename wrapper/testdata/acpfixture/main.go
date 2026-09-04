// Command acpfixture is a deterministic ACP agent used by wrapper subprocess
// integration tests. It supports both newline-delimited stdio and Copilot's
// TCP launch shape; it is not installed or linked into the library.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

func main() {
	tracePath := os.Getenv("ACP_FIXTURE_TRACE")
	trace, err := os.OpenFile(tracePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer func() {
		_, _ = fmt.Fprintln(trace, "fixture-cleanup")
		_ = trace.Close()
	}()

	port := 0
	for i, arg := range os.Args[1:] {
		if arg == "--port" && i+2 < len(os.Args) {
			port, _ = strconv.Atoi(os.Args[i+2])
		}
	}
	if port == 0 {
		serve(os.Stdin, os.Stdout, trace)
		return
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	serve(conn, conn, trace)
}

func serve(r io.Reader, w io.Writer, trace io.Writer) {
	scanner := bufio.NewScanner(r)
	encoder := json.NewEncoder(w)
	promptCount := 0
	var heldID any
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		_, _ = trace.Write(append(line, '\n'))
		var frame struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &frame) != nil {
			continue
		}
		switch frame.Method {
		case "initialize":
			respond(encoder, frame.ID, map[string]any{
				"protocolVersion": 1,
				"authMethods":     []map[string]any{{"id": "fixture-auth", "name": "Fixture", "type": "agent"}},
				"agentCapabilities": map[string]any{
					"loadSession":         true,
					"sessionCapabilities": map[string]any{"close": map[string]any{}},
				},
			})
		case "authenticate", "session/set_mode", "session/set_config_option", "session/close":
			respond(encoder, frame.ID, map[string]any{})
		case "session/load":
			respond(encoder, frame.ID, map[string]any{"sessionId": "resume-tcp"})
		case "session/new":
			respond(encoder, frame.ID, map[string]any{"sessionId": "fresh-tcp"})
		case "session/prompt":
			promptCount++
			if promptCount == 1 {
				heldID = frame.ID
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{
						"sessionId": "resume-tcp",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "tcp delta"},
						},
					},
				})
				_ = encoder.Encode(map[string]any{
					"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{
						"sessionId": "resume-tcp",
						"update": map[string]any{
							"sessionUpdate": "tool_call", "toolCallId": "tool-tcp",
							"title": "TCP Fixture Tool", "kind": "other", "status": "in_progress",
							"rawInput": map[string]any{"path": "/tmp/example"},
						},
					},
				})
			} else {
				respond(encoder, frame.ID, map[string]any{"stopReason": "end_turn"})
			}
		case "session/cancel":
			if heldID != nil {
				respond(encoder, heldID, map[string]any{"stopReason": "cancelled"})
				heldID = nil
			}
		}
	}
}

func respond(encoder *json.Encoder, id, result any) {
	_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

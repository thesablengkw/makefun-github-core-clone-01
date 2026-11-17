// MCP endpoint: JSON-RPC 2.0 over POST /mcp, plain JSON responses.
// Six tools, byte-identical tools/list output, and the discover /
// initialize handshake the conformance client expects.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

type mcpHandlers struct {
	store     *Store
	toolsJSON json.RawMessage
}

func newMCPHandlers(s *Store) *mcpHandlers {
	h := &mcpHandlers{store: s}
	tools := []map[string]any{
		mcpTool("create_repo", "Create a repository for the viewer.", map[string]any{
			"name":        map[string]any{"type": "string", "description": "Repository name."},
			"description": map[string]any{"type": "string", "description": "Repository description."},
			"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
		}, []string{"name"}),
		mcpTool("get_repo", "Get a repository by owner and name.", map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
		}, []string{"owner", "repo"}),
		mcpTool("create_issue", "Create an issue in a repository.", map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"title": map[string]any{"type": "string", "description": "Issue title."},
			"body":  map[string]any{"type": "string", "description": "Issue body."},
		}, []string{"owner", "repo", "title"}),
		mcpTool("list_issues", "List issues in a repository.", map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"state": map[string]any{"type": "string", "description": "Issue state: open, closed, or all."},
		}, []string{"owner", "repo"}),
		mcpTool("create_pull_request", "Create a pull request.", map[string]any{
			"owner": map[string]any{"type": "string", "description": "Repository owner."},
			"repo":  map[string]any{"type": "string", "description": "Repository name."},
			"title": map[string]any{"type": "string", "description": "Pull request title."},
			"head":  map[string]any{"type": "string", "description": "Head branch name."},
			"base":  map[string]any{"type": "string", "description": "Base branch name."},
		}, []string{"owner", "repo", "title", "head", "base"}),
		mcpTool("merge_pull_request", "Merge a pull request.", map[string]any{
			"owner":       map[string]any{"type": "string", "description": "Repository owner."},
			"repo":        map[string]any{"type": "string", "description": "Repository name."},
			"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
		}, []string{"owner", "repo", "pull_number"}),
	}
	// Render once at startup: tools/list must be byte-identical.
	b, _ := json.Marshal(map[string]any{"tools": tools})
	h.toolsJSON = b
	return h
}

func mcpTool(name, desc string, props map[string]any, required []string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": desc,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}
}

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": "forge", "version": "1.0.0"}
}

// ServeHTTP handles POST /mcp (JSON-RPC) and DELETE /mcp (close).
func (h *mcpHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &req); err != nil {
		// Parse error: HTTP 400 with a JSON-RPC -32700 body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"},
		})
		return
	}
	id, hasID := req["id"]
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)

	// Notifications carry no id and expect no response body.
	if !hasID {
		if method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	reply := func(result any) {
		resp := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
		writeJSON(w, http.StatusOK, resp)
	}
	rpcErr := func(code int, msg string) {
		resp := map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": msg},
		}
		writeJSON(w, http.StatusOK, resp)
	}

	switch method {
	case "server/discover":
		reply(map[string]any{
			"supportedVersions": mcpVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo(),
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": mcpServerInfo(),
			},
		})
	case "initialize":
		version := "2025-03-26"
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			version = v
		}
		reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo(),
		})
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Byte-identical across calls: the pre-rendered document.
		var out bytes.Buffer
		out.WriteString(`{"jsonrpc":"2.0","id":`)
		idBytes, _ := json.Marshal(id)
		out.Write(idBytes)
		out.WriteString(`,"result":`)
		out.Write(h.toolsJSON)
		out.WriteString(`}`)
		w.Write(out.Bytes())
	case "tools/call":
		h.toolsCall(w, id, params, rpcErr)
	default:
		rpcErr(-32601, "Method not found: "+method)
	}
}

// toolsCall executes one tool call.
func (h *mcpHandlers) toolsCall(w http.ResponseWriter, id any, params map[string]any, rpcErr func(int, string)) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Unknown tool: -32601, never HTTP 500.
	known := map[string]bool{
		"create_repo": true, "get_repo": true, "create_issue": true,
		"list_issues": true, "create_pull_request": true, "merge_pull_request": true,
	}
	if !known[name] {
		rpcErr(-32601, "Unknown tool: "+name)
		return
	}
	// Required arguments.
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"create_issue":        {"owner", "repo", "title"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	for _, key := range required[name] {
		if _, ok := args[key]; !ok {
			rpcErr(-32602, "Missing required argument: "+key)
			return
		}
	}

	result, isError := h.runTool(name, args)
	text, _ := json.Marshal(result)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(text)},
			},
			"structuredContent": result,
		},
	}
	if isError {
		resp["result"].(map[string]any)["isError"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// runTool executes a tool against the store. It returns the result object
// and whether the call failed at runtime (reported as isError, HTTP 200).
func (h *mcpHandlers) runTool(name string, args map[string]any) (map[string]any, bool) {
	strArg := func(key string) string {
		if s, ok := args[key].(string); ok {
			return s
		}
		return ""
	}
	intArg := func(key string) int {
		switch v := args[key].(type) {
		case float64:
			return int(v)
		case int64:
			return int(v)
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		}
		return 0
	}

	switch name {
	case "create_repo":
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		repo := h.store.CreateRepo(viewerLogin, strArg("name"), strArg("description"), false)
		if b, ok := args["private"].(bool); ok && repo != nil {
			repo.Private = b
			repo.rebuildRepoJSON(h.store)
		}
		if repo == nil {
			return map[string]any{"error": "Repository already exists"}, true
		}
		var v map[string]any
		json.Unmarshal(repo.cachedJSON, &v)
		return v, false
	case "get_repo":
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		repo := h.store.GetRepo(strArg("owner"), strArg("repo"))
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		var v map[string]any
		json.Unmarshal(repo.cachedJSON, &v)
		return v, false
	case "create_issue":
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		repo := h.store.GetRepo(strArg("owner"), strArg("repo"))
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		is := h.store.CreateIssue(repo, strArg("title"), strArg("body"), nil)
		var v map[string]any
		json.Unmarshal(is.cachedJSON, &v)
		return v, false
	case "list_issues":
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		repo := h.store.GetRepo(strArg("owner"), strArg("repo"))
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		state := strArg("state")
		if state == "" {
			state = "open"
		}
		var items []map[string]any
		for _, is := range repo.Issues {
			if state != "all" && is.State != state {
				continue
			}
			var v map[string]any
			json.Unmarshal(is.cachedJSON, &v)
			items = append(items, v)
		}
		if items == nil {
			items = []map[string]any{}
		}
		return map[string]any{"issues": items}, false
	case "create_pull_request":
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		repo := h.store.GetRepo(strArg("owner"), strArg("repo"))
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		if repo.Ref("refs/heads/"+strArg("head")) == "" {
			return map[string]any{"error": "Head branch does not exist"}, true
		}
		if repo.OpenPRForBranch(strArg("head"), strArg("base")) != nil {
			return map[string]any{"error": "A pull request already exists for this head"}, true
		}
		pr := h.store.CreatePR(repo, strArg("title"), "", strArg("head"), strArg("base"))
		var v map[string]any
		json.Unmarshal(pr.cachedJSON, &v)
		return v, false
	case "merge_pull_request":
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		repo := h.store.GetRepo(strArg("owner"), strArg("repo"))
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		pr := repo.GetPR(intArg("pull_number"))
		if pr == nil {
			return map[string]any{"error": "Pull request not found"}, true
		}
		if pr.State != "open" {
			return map[string]any{"error": "Pull request is already closed"}, true
		}
		if h.store.MergePR(repo, pr) == "" {
			return map[string]any{"error": "Pull request is conflicted"}, true
		}
		var v map[string]any
		json.Unmarshal(pr.cachedJSON, &v)
		return v, false
	}
	return map[string]any{"error": "Unknown tool"}, true
}

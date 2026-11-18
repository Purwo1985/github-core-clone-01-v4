// MCP: JSON-RPC 2.0 Streamable HTTP at POST /mcp, modern (2026-07-28)
// discovery plus the legacy initialize handshake, and six tools.
package main

import (
	"strconv"
	"strings"
)

var mcpToolsJSON []byte

func initMCPTools() {
	b := make([]byte, 0, 2048)
	b = append(b, `{"tools":[`...)
	tool := func(name, desc string, props [][3]string, required []string) {
		b = append(b, `{"name":`...)
		b = appendJSONString(b, name)
		b = append(b, `,"description":`...)
		b = appendJSONString(b, desc)
		b = append(b, `,"inputSchema":{"type":"object","properties":{`...)
		for i, p := range props {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, p[0])
			b = append(b, ':')
			b = append(b, `{"type":`...)
			b = appendJSONString(b, p[1])
			if p[2] != "" {
				b = append(b, `,"description":`...)
				b = appendJSONString(b, p[2])
			}
			b = append(b, `}`...)
		}
		b = append(b, `},"required":[`...)
		for i, r := range required {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, r)
		}
		b = append(b, `]}}`...)
	}
	tool("create_repo", "Create a repository owned by the viewer.", [][3]string{
		{"owner", "string", "Owner login"},
		{"name", "string", "Repository name"},
		{"description", "string", "Repository description"},
		{"private", "boolean", "Whether the repository is private"},
	}, []string{"name"})
	b = append(b, ',')
	tool("get_repo", "Get a repository by owner and name.", [][3]string{
		{"owner", "string", "Owner login"},
		{"repo", "string", "Repository name"},
		{"full_name", "string", "Full repository name owner/repo"},
	}, []string{"owner", "repo"})
	b = append(b, ',')
	tool("create_issue", "Create an issue in a repository.", [][3]string{
		{"owner", "string", "Owner login"},
		{"repo", "string", "Repository name"},
		{"title", "string", "Issue title"},
		{"body", "string", "Issue body"},
	}, []string{"owner", "repo", "title"})
	b = append(b, ',')
	tool("list_issues", "List issues in a repository.", [][3]string{
		{"owner", "string", "Owner login"},
		{"repo", "string", "Repository name"},
		{"state", "string", "Issue state: open, closed or all"},
	}, []string{"owner", "repo"})
	b = append(b, ',')
	tool("create_pull_request", "Create a pull request.", [][3]string{
		{"owner", "string", "Owner login"},
		{"repo", "string", "Repository name"},
		{"title", "string", "Pull request title"},
		{"body", "string", "Pull request body"},
		{"head", "string", "Head branch name"},
		{"base", "string", "Base branch name"},
	}, []string{"owner", "repo", "title", "head", "base"})
	b = append(b, ',')
	tool("merge_pull_request", "Merge a pull request with squash.", [][3]string{
		{"owner", "string", "Owner login"},
		{"repo", "string", "Repository name"},
		{"pull_number", "integer", "Pull request number"},
	}, []string{"owner", "repo", "pull_number"})
	b = append(b, `]}`...)
	mcpToolsJSON = b
}

// handleMCP answers POST /mcp.
func handleMCP(req *request) *response {
	if req.method != "POST" {
		return &response{status: 405, body: jsonError("method not allowed"), extra: oauthScopes()}
	}
	body, ok := parseJSON(req.body)
	if !ok {
		// Parse error: HTTP 400, JSON-RPC -32700, id null.
		return &response{status: 400, body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	if body.kind != 'o' {
		return &response{status: 400, body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`)}
	}
	idVal := body.get("id")
	method := body.get("method").strOr("")
	storeMu()
	defer storeUnlock()
	// Notifications (no id) get an empty 202.
	if idVal == nil {
		if method == "notifications/initialized" {
			return &response{status: 202, body: []byte(`{}`)}
		}
		return &response{status: 202, body: []byte(`{}`)}
	}
	idJSON := renderID(idVal)
	switch method {
	case "server/discover":
		res := `{"supportedVersions":["2026-07-28","2025-11-25"],"capabilities":{"tools":{}},"serverInfo":{"name":"github-core-clone-01-v4","version":"1.0.0"},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"github-core-clone-01-v4","version":"1.0.0"}}}`
		return rpcResult(idJSON, []byte(res))
	case "initialize":
		params := body.get("params")
		proto := params.get("protocolVersion").strOr("2025-11-25")
		res := `{"protocolVersion":` + jsonString(proto) + `,"capabilities":{"tools":{}},"serverInfo":{"name":"github-core-clone-01-v4","version":"1.0.0"}}`
		return rpcResult(idJSON, []byte(res))
	case "tools/list":
		return rpcResult(idJSON, mcpToolsJSON)
	case "tools/call":
		return mcpCallTool(idJSON, body.get("params"))
	case "ping":
		return rpcResult(idJSON, []byte(`{}`))
	default:
		return rpcError(idJSON, -32601, "Method not found")
	}
}

func jsonString(s string) string {
	b := appendJSONString(nil, s)
	return string(b)
}

func renderID(v *jval) string {
	if v == nil {
		return "null"
	}
	switch v.kind {
	case 's':
		return jsonString(v.str)
	case 'n':
		return strconv.Itoa(int(v.num))
	}
	return "null"
}

func rpcResult(idJSON string, result []byte) *response {
	b := make([]byte, 0, len(result)+32)
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, idJSON...)
	b = append(b, `,"result":`...)
	b = append(b, result...)
	b = append(b, '}')
	return &response{status: 200, body: b}
}

func rpcError(idJSON string, code int, msg string) *response {
	b := make([]byte, 0, 96)
	b = append(b, `{"jsonrpc":"2.0","id":`...)
	b = append(b, idJSON...)
	b = append(b, `,"error":{"code":`...)
	b = appendInt(b, code)
	b = append(b, `,"message":`...)
	b = appendJSONString(b, msg)
	b = append(b, `}}`...)
	return &response{status: 200, body: b}
}

// mcpCallTool dispatches one tools/call request.
func mcpCallTool(idJSON string, params *jval) *response {
	name := params.get("name").strOr("")
	args := params.get("arguments")
	// Normalize the tool name (suffix match, case-insensitive).
	norm := strings.ToLower(name)
	if i := strings.LastIndexByte(norm, '/'); i >= 0 {
		norm = norm[i+1:]
	}
	// Look up the tool; unknown tools get -32602.
	known := map[string]bool{
		"create_repo": true, "get_repo": true, "create_issue": true,
		"list_issues": true, "create_pull_request": true, "merge_pull_request": true,
	}
	if !known[norm] {
		return rpcError(idJSON, -32602, "Unknown tool: "+name)
	}
	// Required-argument check.
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"create_issue":        {"owner", "repo", "title"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	for _, key := range required[norm] {
		if v := args.get(key); v == nil || v.kind == 'z' {
			// Tolerate full_name for owner/repo.
			if (key == "owner" || key == "repo") && args.get("full_name") != nil {
				continue
			}
			if key == "name" && args.get("repo") != nil {
				continue
			}
			return rpcError(idJSON, -32602, "Missing required argument: "+key)
		}
	}
	owner, repo := mcpRepo(args)
	var result []byte
	switch norm {
	case "create_repo":
		result = mcpCreateRepo(args)
		if result == nil {
			return rpcError(idJSON, -32602, "Repository already exists")
		}
	case "get_repo":
		result = mcpGetRepo(owner, repo)
		if result == nil {
			return rpcError(idJSON, -32602, "Repository not found")
		}
	case "create_issue":
		result = mcpCreateIssue(args, owner, repo)
		if result == nil {
			return rpcError(idJSON, -32602, "Repository not found")
		}
	case "list_issues":
		result = mcpListIssues(args, owner, repo)
		if result == nil {
			return rpcError(idJSON, -32602, "Repository not found")
		}
	case "create_pull_request":
		result = mcpCreatePull(args, owner, repo)
		if result == nil {
			return rpcError(idJSON, -32602, "Pull request could not be created")
		}
	case "merge_pull_request":
		result = mcpMergePull(args, owner, repo)
		if result == nil {
			return rpcError(idJSON, -32602, "Pull request could not be merged")
		}
	}
	// Envelope: content text + structuredContent.
	b := make([]byte, 0, len(result)+128)
	b = append(b, `{"content":[{"type":"text","text":`...)
	b = appendJSONString(b, string(result))
	b = append(b, `}],"structuredContent":`...)
	b = append(b, result...)
	b = append(b, '}')
	return rpcResult(idJSON, b)
}

// mcpRepo extracts owner/repo, tolerating full_name.
func mcpRepo(args *jval) (string, string) {
	owner := args.get("owner").strOr("")
	repo := args.get("repo").strOr("")
	if full := args.get("full_name").strOr(""); full != "" {
		if i := strings.IndexByte(full, '/'); i >= 0 {
			if owner == "" {
				owner = full[:i]
			}
			if repo == "" {
				repo = full[i+1:]
			}
		}
	}
	return owner, repo
}

func mcpCreateRepo(args *jval) []byte {
	name := args.get("name").strOr("")
	if name == "" {
		name = args.get("repo").strOr("")
	}
	if name == "" {
		return nil
	}
	if gstore.getRepo(name) != nil {
		return nil
	}
	desc := args.get("description").strOr("")
	priv := args.get("private").boolOr(false)
	r := gstore.createRepo(name, desc, priv)
	return r.json
}

func mcpGetRepo(owner, repo string) []byte {
	if owner != viewer.login {
		return nil
	}
	r := gstore.getRepo(repo)
	if r == nil {
		return nil
	}
	return r.json
}

func mcpCreateIssue(args *jval, owner, repo string) []byte {
	if owner != viewer.login {
		return nil
	}
	r := gstore.getRepo(repo)
	if r == nil {
		return nil
	}
	title := args.get("title").strOr("")
	if title == "" {
		return nil
	}
	is := &issue{
		num:   r.nextIssue,
		id:    gstore.nextIssueID,
		title: title,
		body:  args.get("body").strOr(""),
		state: "open",
		seq:   len(r.issues),
	}
	is.gid = gidIssue(r.id, is.num)
	gstore.nextIssueID++
	r.nextIssue++
	r.issues = append(r.issues, is)
	r.issueByNum[is.num] = is
	is.renderJSON(r)
	return is.json
}

func mcpListIssues(args *jval, owner, repo string) []byte {
	if owner != viewer.login {
		return nil
	}
	r := gstore.getRepo(repo)
	if r == nil {
		return nil
	}
	state := args.get("state").strOr("open")
	b := make([]byte, 0, 512)
	b = append(b, '[')
	first := true
	for _, is := range r.issues {
		if state != "all" && is.state != state {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, is.json...)
	}
	b = append(b, ']')
	return b
}

func mcpCreatePull(args *jval, owner, repo string) []byte {
	if owner != viewer.login {
		return nil
	}
	r := gstore.getRepo(repo)
	if r == nil {
		return nil
	}
	head := args.get("head").strOr("")
	base := args.get("base").strOr("")
	title := args.get("title").strOr("")
	headTip, ok1 := r.refs["refs/heads/"+head]
	baseTip, ok2 := r.refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return nil
	}
	p := &pull{
		num:     r.nextPull,
		id:      gstore.nextPullID,
		title:   title,
		body:    args.get("body").strOr(""),
		headRef: head,
		baseRef: base,
		headSha: headTip,
		baseSha: baseTip,
		state:   "open",
		seq:     len(r.pulls),
	}
	p.gid = gidPull(r.id, p.num)
	gstore.nextPullID++
	r.nextPull++
	_, conflict := r.threeWayMerge(baseTip, headTip)
	p.mergeable = !conflict
	mb, _ := r.mergeBase(baseTip, headTip)
	mbTree := oid{}
	if o := r.objs[mb]; o != nil && o.typ == objCommit {
		mbTree = parseCommit(o.data).tree
	}
	headTree := oid{}
	if o := r.objs[headTip]; o != nil && o.typ == objCommit {
		headTree = parseCommit(o.data).tree
	}
	p.files = r.diffFiles(mbTree, headTree)
	r.pulls = append(r.pulls, p)
	r.pullByNum[p.num] = p
	p.renderJSON(r)
	return p.json
}

func mcpMergePull(args *jval, owner, repo string) []byte {
	if owner != viewer.login {
		return nil
	}
	r := gstore.getRepo(repo)
	if r == nil {
		return nil
	}
	num := args.get("pull_number").intOr(0)
	p := r.pullByNum[num]
	if p == nil || p.state != "open" {
		return nil
	}
	baseTip, ok1 := r.refs["refs/heads/"+p.baseRef]
	headTip, ok2 := r.refs["refs/heads/"+p.headRef]
	if !ok1 || !ok2 {
		return nil
	}
	mergedTree, conflict := r.threeWayMerge(baseTip, headTip)
	if conflict {
		return nil
	}
	msg := "Merge pull request #" + itoa(p.num) + " from " + p.headRef + "\n"
	ident := viewer.name + " <" + viewer.email + "> " + itoa64(cfg.trialNow) + " +0000"
	data := buildCommit(mergedTree, []oid{baseTip}, ident, ident, msg)
	mergeID := hashObject(objCommit, data)
	r.objs[mergeID] = &obj{typ: objCommit, data: data}
	r.refs["refs/heads/"+p.baseRef] = mergeID
	p.state = "closed"
	p.merged = true
	p.mergedAt = trialISO
	p.mergeSha = mergeID
	p.mergeable = true
	p.renderJSON(r)
	r.bump()
	b := make([]byte, 0, 128)
	b = append(b, `{"sha":`...)
	b = appendJSONString(b, mergeID.hex())
	b = append(b, `,"merged":true,"number":`...)
	b = appendInt(b, p.num)
	b = append(b, `,"message":`...)
	b = appendJSONString(b, "Pull request successfully merged")
	b = append(b, `}`...)
	return b
}

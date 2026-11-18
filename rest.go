// REST v3 API: /api/v3/* — repos, contents, labels, issues, comments,
// pull requests, refs, branches and commits.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

var gstore *store

// handleREST routes /api/v3/... requests. The store lock is held for the
// whole handler (state operations are microseconds; no I/O inside).
func handleREST(req *request) *response {
	storeMu()
	defer storeUnlock()
	seg := req.seg[2:] // after api/v3
	if len(seg) == 0 {
		if req.method == "GET" {
			return &response{status: 200, body: []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}","user_repositories_url":"/api/v3/user/repos"}`), extra: oauthScopes()}
		}
		return notFound()
	}
	switch seg[0] {
	case "meta":
		if req.method == "GET" {
			return &response{status: 200, body: []byte(`{"installed_version":"1.0.0"}`), extra: oauthScopes()}
		}
	case "user":
		return handleUser(req, seg[1:])
	case "repos":
		return handleRepos(req, seg[1:])
	}
	return notFound()
}

func oauthScopes() [][2]string {
	return [][2]string{{"X-OAuth-Scopes", "repo, read:org"}}
}

func notFound() *response {
	return &response{status: 404, body: jsonError("Not Found"), extra: oauthScopes()}
}

func badJSON() *response {
	return &response{status: 400, body: jsonError("Problems parsing JSON"), extra: oauthScopes()}
}

// parseBody parses the request body as JSON. Returns nil, false on failure.
func parseBody(req *request) (*jval, bool) {
	if len(req.body) == 0 {
		return &jval{kind: 'o', obj: make(map[string]*jval)}, true
	}
	v, ok := parseJSON(req.body)
	if !ok {
		return nil, false
	}
	return v, true
}

// lookupRepo resolves /repos/{owner}/{repo}; owner must be the viewer.
func lookupRepo(seg []string) (*Repo, bool) {
	if len(seg) < 2 {
		return nil, false
	}
	if seg[0] != viewer.login {
		return nil, false
	}
	r := gstore.getRepo(seg[1])
	if r == nil {
		return nil, false
	}
	return r, true
}

// handleUser routes /api/v3/user and /api/v3/user/repos.
func handleUser(req *request, seg []string) *response {
	if len(seg) == 0 {
		if req.method != "GET" {
			return notFound()
		}
		b := make([]byte, 0, 128)
		b = append(b, `{"login":`...)
		b = appendJSONString(b, viewer.login)
		b = append(b, `,"id":`...)
		b = appendInt(b, viewer.id)
		b = append(b, `,"name":`...)
		b = appendJSONString(b, viewer.name)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, viewer.email)
		b = append(b, `,"type":"User"}`...)
		return &response{status: 200, body: b, extra: oauthScopes()}
	}
	if seg[0] == "repos" {
		switch req.method {
		case "GET":
			return listUserRepos(req)
		case "POST":
			return createUserRepo(req)
		}
	}
	return notFound()
}

func listUserRepos(req *request) *response {
	page, extra := paginate(gstore.order, req, "/api/v3/user/repos")
	b := make([]byte, 0, 1024)
	b = append(b, '[')
	for i, r := range page {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, r.json...)
	}
	b = append(b, ']')
	return &response{status: 200, body: b, extra: append(oauthScopes(), extra...)}
}

func createUserRepo(req *request) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	name := body.get("name").strOr("")
	if name == "" {
		return &response{status: 422, body: jsonError("name is required"), extra: oauthScopes()}
	}
	if gstore.getRepo(name) != nil {
		return &response{status: 422, body: jsonError("Repository already exists on this account."), extra: oauthScopes()}
	}
	desc := body.get("description").strOr("")
	priv := body.get("private").boolOr(false)
	r := gstore.createRepo(name, desc, priv)
	return &response{status: 201, body: r.json, extra: oauthScopes()}
}

// handleRepos routes /api/v3/repos/{owner}/{repo}/...
func handleRepos(req *request, seg []string) *response {
	r, ok := lookupRepo(seg)
	if !ok {
		return notFound()
	}
	rest := seg[2:]
	if len(rest) == 0 {
		switch req.method {
		case "GET":
			return &response{status: 200, body: r.json, extra: oauthScopes()}
		case "PATCH":
			body, ok := parseBody(req)
			if !ok {
				return badJSON()
			}
			if d := body.get("description"); d != nil && d.kind == 's' {
				r.desc = d.str
			}
			if p := body.get("private"); p != nil && p.kind == 'b' {
				r.priv = p.num != 0
			}
			r.renderRepoJSON()
			return &response{status: 200, body: r.json, extra: oauthScopes()}
		case "DELETE":
			gstore.deleteRepo(r.name)
			return &response{status: 204, noBody: true, extra: oauthScopes()}
		}
		return notFound()
	}
	switch rest[0] {
	case "contents":
		return handleContents(req, r, rest[1:])
	case "labels":
		return handleLabels(req, r)
	case "issues":
		return handleIssues(req, r, rest[1:])
	case "pulls":
		return handlePulls(req, r, rest[1:])
	case "branches":
		return handleBranches(req, r, rest[1:])
	case "git":
		return handleGitAPI(req, r, rest[1:])
	case "commits":
		return handleCommits(req, r)
	}
	return notFound()
}

// --- contents -------------------------------------------------------------

// contentsPath joins the remaining segments into a file path.
func contentsPath(seg []string) string {
	return strings.Join(seg, "/")
}

func handleContents(req *request, r *Repo, seg []string) *response {
	if len(seg) == 0 {
		return notFound()
	}
	path := contentsPath(seg)
	switch req.method {
	case "GET":
		return getContents(req, r, path)
	case "PUT":
		return putContents(req, r, path)
	case "DELETE":
		return deleteContents(req, r, path)
	}
	return notFound()
}

func getContents(req *request, r *Repo, path string) *response {
	refName := r.defaultBranch
	if v, ok := queryParam(req.query, "ref"); ok && v != "" {
		refName = v
	}
	tip, ok := r.refs["refs/heads/"+refName]
	if !ok {
		return notFound()
	}
	commit := r.objs[tip]
	if commit == nil || commit.typ != objCommit {
		return notFound()
	}
	tree := parseCommit(commit.data).tree
	mode, id, ok := r.getFile(tree, path)
	if !ok {
		// Maybe a directory.
		ents, ok := r.listDir(tree, path)
		if !ok {
			return notFound()
		}
		b := make([]byte, 0, 256)
		b = append(b, '[')
		for i, e := range ents {
			if i > 0 {
				b = append(b, ',')
			}
			typ := "file"
			if e.mode == "40000" {
				typ = "dir"
			}
			p := path + "/" + e.name
			b = append(b, `{"type":`...)
			b = appendJSONString(b, typ)
			b = append(b, `,"name":`...)
			b = appendJSONString(b, e.name)
			b = append(b, `,"path":`...)
			b = appendJSONString(b, p)
			b = append(b, `,"sha":`...)
			b = appendJSONString(b, e.id.hex())
			b = append(b, `}`...)
		}
		b = append(b, ']')
		return &response{status: 200, body: b, extra: oauthScopes()}
	}
	if mode == "40000" {
		ents, _ := r.listDir(tree, path)
		b := make([]byte, 0, 256)
		b = append(b, '[')
		for i, e := range ents {
			if i > 0 {
				b = append(b, ',')
			}
			typ := "file"
			if e.mode == "40000" {
				typ = "dir"
			}
			p := path + "/" + e.name
			b = append(b, `{"type":`...)
			b = appendJSONString(b, typ)
			b = append(b, `,"name":`...)
			b = appendJSONString(b, e.name)
			b = append(b, `,"path":`...)
			b = appendJSONString(b, p)
			b = append(b, `,"sha":`...)
			b = appendJSONString(b, e.id.hex())
			b = append(b, `}`...)
		}
		b = append(b, ']')
		return &response{status: 200, body: b, extra: oauthScopes()}
	}
	blob := r.objs[id]
	if blob == nil {
		return notFound()
	}
	b := make([]byte, 0, len(blob.data)+128)
	b = append(b, `{"type":"file","encoding":"base64","name":`...)
	b = appendJSONString(b, lastSegment(path))
	b = append(b, `,"path":`...)
	b = appendJSONString(b, path)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, id.hex())
	b = append(b, `,"size":`...)
	b = appendInt(b, len(blob.data))
	b = append(b, `,"content":`...)
	b = appendJSONString(b, base64.StdEncoding.EncodeToString(blob.data))
	b = append(b, `}`...)
	return &response{status: 200, body: b, extra: oauthScopes()}
}

func lastSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// makeCommit creates a commit on branch with the given tree and message,
// and returns its id.
func (r *Repo) makeCommit(branch string, tree oid, message string) oid {
	var parents []oid
	if tip, ok := r.refs["refs/heads/"+branch]; ok {
		parents = append(parents, tip)
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	ident := viewer.name + " <" + viewer.email + "> " + strconv.FormatInt(cfg.trialNow, 10) + " +0000"
	data := buildCommit(tree, parents, ident, ident, message)
	id := hashObject(objCommit, data)
	r.objs[id] = &obj{typ: objCommit, data: data}
	r.refs["refs/heads/"+branch] = id
	return id
}

func putContents(req *request, r *Repo, path string) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	contentB64 := body.get("content").strOr("")
	raw, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return &response{status: 422, body: jsonError("content is not valid base64"), extra: oauthScopes()}
	}
	message := body.get("message").strOr("")
	branch := body.get("branch").strOr(r.defaultBranch)
	if branch == "" {
		branch = r.defaultBranch
	}
	shaField := body.get("sha")
	// Resolve the branch tip (create the branch from the default tip when
	// it does not exist yet, like GitHub does for contents writes).
	var baseTree oid
	var isCreate bool
	if tip, ok := r.refs["refs/heads/"+branch]; ok {
		if o := r.objs[tip]; o != nil && o.typ == objCommit {
			baseTree = parseCommit(o.data).tree
		}
		if _, _, exists := r.getFile(baseTree, path); exists {
			isCreate = false
		} else {
			isCreate = true
		}
	} else {
		// New branch: start from the default tip, or from nothing.
		if tip, ok := r.defaultTip(); ok {
			if o := r.objs[tip]; o != nil && o.typ == objCommit {
				baseTree = parseCommit(o.data).tree
			}
			r.refs["refs/heads/"+branch] = tip
		}
		isCreate = true
	}
	// sha check on update.
	if shaField != nil && shaField.kind == 's' && shaField.str != "" {
		want, ok := parseOIDHex(shaField.str)
		if !ok {
			return &response{status: 422, body: jsonError("sha is not valid"), extra: oauthScopes()}
		}
		_, cur, exists := r.getFile(baseTree, path)
		if !exists || cur != want {
			return &response{status: 409, body: jsonError("sha does not match"), extra: oauthScopes()}
		}
	}
	blobID := hashObject(objBlob, raw)
	r.objs[blobID] = &obj{typ: objBlob, data: raw}
	newTree := r.setFile(baseTree, path, "100644", blobID)
	commitID := r.makeCommit(branch, newTree, message)
	r.bump()
	status := 201
	if !isCreate {
		status = 200
	}
	b := make([]byte, 0, 192)
	b = append(b, `{"content":{"name":`...)
	b = appendJSONString(b, lastSegment(path))
	b = append(b, `,"path":`...)
	b = appendJSONString(b, path)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, blobID.hex())
	b = append(b, `,"size":`...)
	b = appendInt(b, len(raw))
	b = append(b, `,"encoding":"base64"},"commit":{"sha":`...)
	b = appendJSONString(b, commitID.hex())
	b = append(b, `}}`...)
	return &response{status: status, body: b, extra: oauthScopes()}
}

func deleteContents(req *request, r *Repo, path string) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	message := body.get("message").strOr("")
	branch := body.get("branch").strOr(r.defaultBranch)
	if branch == "" {
		branch = r.defaultBranch
	}
	tip, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return notFound()
	}
	commit := r.objs[tip]
	if commit == nil || commit.typ != objCommit {
		return notFound()
	}
	tree := parseCommit(commit.data).tree
	if _, _, exists := r.getFile(tree, path); !exists {
		return notFound()
	}
	newTree, ok := r.deleteFile(tree, path)
	if !ok {
		return notFound()
	}
	commitID := r.makeCommit(branch, newTree, message)
	r.bump()
	b := make([]byte, 0, 96)
	b = append(b, `{"commit":{"sha":`...)
	b = appendJSONString(b, commitID.hex())
	b = append(b, `}}`...)
	return &response{status: 200, body: b, extra: oauthScopes()}
}

// --- labels ---------------------------------------------------------------

func handleLabels(req *request, r *Repo) *response {
	if req.method != "POST" {
		return notFound()
	}
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	name := body.get("name").strOr("")
	if name == "" {
		return &response{status: 422, body: jsonError("name is required"), extra: oauthScopes()}
	}
	color := body.get("color").strOr("d73a4a")
	if _, exists := r.labels[name]; !exists {
		r.labels[name] = &label{name: name, color: color}
	}
	b := make([]byte, 0, 64)
	b = append(b, `{"name":`...)
	b = appendJSONString(b, name)
	b = append(b, `,"color":`...)
	b = appendJSONString(b, color)
	b = append(b, `,"description":""}`...)
	return &response{status: 201, body: b, extra: oauthScopes()}
}

// --- issues ---------------------------------------------------------------

func handleIssues(req *request, r *Repo, seg []string) *response {
	if len(seg) == 0 {
		switch req.method {
		case "GET":
			return listIssues(req, r)
		case "POST":
			return createIssueREST(req, r)
		}
		return notFound()
	}
	num, err := strconv.Atoi(seg[0])
	if err != nil || num < 1 {
		return notFound()
	}
	is := r.issueByNum[num]
	if is == nil {
		return notFound()
	}
	if len(seg) == 1 {
		switch req.method {
		case "GET":
			return &response{status: 200, body: is.json, extra: oauthScopes()}
		case "PATCH":
			body, ok := parseBody(req)
			if !ok {
				return badJSON()
			}
			if st := body.get("state"); st != nil && st.kind == 's' && (st.str == "open" || st.str == "closed") {
				is.state = st.str
			}
			if t := body.get("title"); t != nil && t.kind == 's' {
				is.title = t.str
			}
			if bd := body.get("body"); bd != nil && bd.kind == 's' {
				is.body = bd.str
			}
			is.renderJSON(r)
			return &response{status: 200, body: is.json, extra: oauthScopes()}
		}
		return notFound()
	}
	if seg[1] == "comments" {
		switch req.method {
		case "GET":
			b := make([]byte, 0, 256)
			b = append(b, '[')
			for i, c := range is.comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, c.json...)
			}
			b = append(b, ']')
			return &response{status: 200, body: b, extra: oauthScopes()}
		case "POST":
			body, ok := parseBody(req)
			if !ok {
				return badJSON()
			}
			c := &comment{id: gstore.nextCommentID, body: body.get("body").strOr(""), seq: len(is.comments)}
			c.gid = gidComment(r.id, is.num, c.id)
			gstore.nextCommentID++
			c.renderJSON(r, is.num)
			is.comments = append(is.comments, c)
			is.renderJSON(r)
			return &response{status: 201, body: c.json, extra: oauthScopes()}
		}
	}
	return notFound()
}

func listIssues(req *request, r *Repo) *response {
	state, _ := queryParam(req.query, "state")
	if state == "" {
		state = "open"
	}
	sortDir := "desc"
	if v, ok := queryParam(req.query, "sort"); ok && v == "created" {
		_ = v
	}
	if v, ok := queryParam(req.query, "direction"); ok && (v == "asc" || v == "desc") {
		sortDir = v
	}
	var filtered []*issue
	for _, is := range r.issues {
		if state == "all" || is.state == state {
			filtered = append(filtered, is)
		}
	}
	if sortDir == "asc" {
		// r.issues is already in creation order.
	} else {
		// Reverse for descending.
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	page, extra := paginate(filtered, req, "/api/v3/repos/"+viewer.login+"/"+r.name+"/issues")
	b := make([]byte, 0, 512)
	b = append(b, '[')
	for i, is := range page {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, is.json...)
	}
	b = append(b, ']')
	return &response{status: 200, body: b, extra: append(oauthScopes(), extra...)}
}

func createIssueREST(req *request, r *Repo) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	title := body.get("title").strOr("")
	if title == "" {
		return &response{status: 422, body: jsonError("title is required"), extra: oauthScopes()}
	}
	// Validate labels.
	var labels []string
	if lv := body.get("labels"); lv != nil && lv.kind == 'a' {
		for _, item := range lv.arr {
			name := ""
			if item.kind == 's' {
				name = item.str
			} else if item.kind == 'o' {
				name = item.get("name").strOr("")
			}
			if name == "" {
				continue
			}
			if _, exists := r.labels[name]; !exists {
				return &response{status: 422, body: jsonError("label does not exist: " + name), extra: oauthScopes()}
			}
			labels = append(labels, name)
		}
	}
	is := &issue{
		num:    r.nextIssue,
		id:     gstore.nextIssueID,
		title:  title,
		body:   body.get("body").strOr(""),
		state:  "open",
		labels: labels,
		seq:    len(r.issues),
	}
	is.gid = gidIssue(r.id, is.num)
	gstore.nextIssueID++
	r.nextIssue++
	r.issues = append(r.issues, is)
	r.issueByNum[is.num] = is
	is.renderJSON(r)
	return &response{status: 201, body: is.json, extra: oauthScopes()}
}

// --- pull requests --------------------------------------------------------

func handlePulls(req *request, r *Repo, seg []string) *response {
	if len(seg) == 0 {
		switch req.method {
		case "GET":
			return listPulls(req, r)
		case "POST":
			return createPullREST(req, r)
		}
		return notFound()
	}
	num, err := strconv.Atoi(seg[0])
	if err != nil || num < 1 {
		return notFound()
	}
	p := r.pullByNum[num]
	if p == nil {
		return notFound()
	}
	if len(seg) == 1 {
		if req.method == "GET" {
			return &response{status: 200, body: p.json, extra: oauthScopes()}
		}
		return notFound()
	}
	switch seg[1] {
	case "files":
		if req.method != "GET" {
			return notFound()
		}
		b := make([]byte, 0, 256)
		b = append(b, '[')
		for i, f := range p.files {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, `{"filename":`...)
			b = appendJSONString(b, f.name)
			b = append(b, `,"status":`...)
			b = appendJSONString(b, f.status)
			b = append(b, `,"additions":`...)
			b = appendInt(b, f.additions)
			b = append(b, `,"deletions":`...)
			b = appendInt(b, f.deletions)
			b = append(b, `,"changes":`...)
			b = appendInt(b, f.additions+f.deletions)
			b = append(b, `}`...)
		}
		b = append(b, ']')
		return &response{status: 200, body: b, extra: oauthScopes()}
	case "merge":
		if req.method != "PUT" {
			return notFound()
		}
		return mergePullREST(req, r, p)
	}
	return notFound()
}

func listPulls(req *request, r *Repo) *response {
	state, _ := queryParam(req.query, "state")
	if state == "" {
		state = "open"
	}
	var filtered []*pull
	for _, p := range r.pulls {
		if state == "all" || p.state == state {
			filtered = append(filtered, p)
		}
	}
	page, extra := paginate(filtered, req, "/api/v3/repos/"+viewer.login+"/"+r.name+"/pulls")
	b := make([]byte, 0, 512)
	b = append(b, '[')
	for i, p := range page {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, p.json...)
	}
	b = append(b, ']')
	return &response{status: 200, body: b, extra: append(oauthScopes(), extra...)}
}

func createPullREST(req *request, r *Repo) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	head := body.get("head").strOr("")
	base := body.get("base").strOr("")
	title := body.get("title").strOr("")
	if head == "" || base == "" {
		return &response{status: 422, body: jsonError("head and base are required"), extra: oauthScopes()}
	}
	headTip, ok1 := r.refs["refs/heads/"+head]
	baseTip, ok2 := r.refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return &response{status: 422, body: jsonError("head or base branch does not exist"), extra: oauthScopes()}
	}
	p := &pull{
		num:     r.nextPull,
		id:      gstore.nextPullID,
		title:   title,
		body:    body.get("body").strOr(""),
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
	// Compute mergeability and the file list now.
	_, conflict := r.threeWayMerge(baseTip, headTip)
	p.mergeable = !conflict
	baseTree, headTree := oid{}, oid{}
	if o := r.objs[baseTip]; o != nil && o.typ == objCommit {
		baseTree = parseCommit(o.data).tree
	}
	if o := r.objs[headTip]; o != nil && o.typ == objCommit {
		headTree = parseCommit(o.data).tree
	}
	// Files are diffed against the merge base.
	mb, _ := r.mergeBase(baseTip, headTip)
	mbTree := oid{}
	if o := r.objs[mb]; o != nil && o.typ == objCommit {
		mbTree = parseCommit(o.data).tree
	}
	_ = baseTree
	p.files = r.diffFiles(mbTree, headTree)
	r.pulls = append(r.pulls, p)
	r.pullByNum[p.num] = p
	p.renderJSON(r)
	return &response{status: 201, body: p.json, extra: oauthScopes()}
}

func mergePullREST(req *request, r *Repo, p *pull) *response {
	if p.state != "open" {
		return &response{status: 405, body: jsonError("pull request is already closed"), extra: oauthScopes()}
	}
	baseTip, ok1 := r.refs["refs/heads/"+p.baseRef]
	headTip, ok2 := r.refs["refs/heads/"+p.headRef]
	if !ok1 || !ok2 {
		return notFound()
	}
	mergedTree, conflict := r.threeWayMerge(baseTip, headTip)
	if conflict {
		return &response{status: 405, body: jsonError("merge conflict"), extra: oauthScopes()}
	}
	// Squash commit on the base branch.
	msg := "Merge pull request #" + itoa(p.num) + " from " + p.headRef + "\n"
	ident := viewer.name + " <" + viewer.email + "> " + strconv.FormatInt(cfg.trialNow, 10) + " +0000"
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
	b = append(b, `,"merged":true,"message":`...)
	b = appendJSONString(b, "Pull request successfully merged")
	b = append(b, `}`...)
	return &response{status: 200, body: b, extra: oauthScopes()}
}

// --- branches, refs, commits ----------------------------------------------

func handleBranches(req *request, r *Repo, seg []string) *response {
	if req.method != "GET" {
		return notFound()
	}
	if len(seg) == 0 {
		b := make([]byte, 0, 256)
		b = append(b, '[')
		names := r.sortedRefNames()
		first := true
		for _, name := range names {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			branch := name[len("refs/heads/"):]
			b = append(b, `{"name":`...)
			b = appendJSONString(b, branch)
			b = append(b, `,"commit":{"sha":`...)
			b = appendJSONString(b, r.refs[name].hex())
			b = append(b, `},"protected":false}`...)
		}
		b = append(b, ']')
		return &response{status: 200, body: b, extra: oauthScopes()}
	}
	branch := strings.Join(seg, "/")
	id, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return notFound()
	}
	b := make([]byte, 0, 128)
	b = append(b, `{"name":`...)
	b = appendJSONString(b, branch)
	b = append(b, `,"commit":{"sha":`...)
	b = appendJSONString(b, id.hex())
	b = append(b, `},"protected":false}`...)
	return &response{status: 200, body: b, extra: oauthScopes()}
}

func handleGitAPI(req *request, r *Repo, seg []string) *response {
	if len(seg) == 0 {
		return notFound()
	}
	switch seg[0] {
	case "refs":
		if req.method == "POST" {
			return createRef(req, r)
		}
		if req.method == "GET" && len(seg) >= 3 && seg[1] == "heads" {
			branch := strings.Join(seg[2:], "/")
			id, ok := r.refs["refs/heads/"+branch]
			if !ok {
				return notFound()
			}
			b := make([]byte, 0, 128)
			b = append(b, `{"ref":`...)
			b = appendJSONString(b, "refs/heads/"+branch)
			b = append(b, `,"object":{"sha":`...)
			b = appendJSONString(b, id.hex())
			b = append(b, `,"type":"commit"}}`...)
			return &response{status: 200, body: b, extra: oauthScopes()}
		}
	}
	return notFound()
}

func createRef(req *request, r *Repo) *response {
	body, ok := parseBody(req)
	if !ok {
		return badJSON()
	}
	ref := body.get("ref").strOr("")
	shaStr := body.get("sha").strOr("")
	id, ok := parseOIDHex(shaStr)
	if !ok || ref == "" || !strings.HasPrefix(ref, "refs/") {
		return &response{status: 422, body: jsonError("ref and sha are required"), extra: oauthScopes()}
	}
	if r.objs[id] == nil {
		return &response{status: 422, body: jsonError("object does not exist"), extra: oauthScopes()}
	}
	if _, exists := r.refs[ref]; exists {
		return &response{status: 422, body: jsonError("Reference already exists"), extra: oauthScopes()}
	}
	r.refs[ref] = id
	r.bump()
	b := make([]byte, 0, 128)
	b = append(b, `{"ref":`...)
	b = appendJSONString(b, ref)
	b = append(b, `,"object":{"sha":`...)
	b = appendJSONString(b, id.hex())
	b = append(b, `,"type":"commit"}}`...)
	return &response{status: 201, body: b, extra: oauthScopes()}
}

func handleCommits(req *request, r *Repo) *response {
	if req.method != "GET" {
		return notFound()
	}
	refName := r.defaultBranch
	if v, ok := queryParam(req.query, "sha"); ok && v != "" {
		refName = v
	}
	tip, ok := r.refs["refs/heads/"+refName]
	if !ok {
		// Maybe a raw sha.
		if id, ok2 := parseOIDHex(refName); ok2 {
			tip = id
			ok = true
		}
	}
	if !ok {
		return notFound()
	}
	// Walk newest first.
	var ids []oid
	seen := make(map[oid]bool)
	queue := []oid{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.objs[cur]
		if o == nil || o.typ != objCommit {
			continue
		}
		ids = append(ids, cur)
		queue = append(queue, parseCommit(o.data).parents...)
	}
	page, extra := paginate(ids, req, "/api/v3/repos/"+viewer.login+"/"+r.name+"/commits")
	b := make([]byte, 0, 512)
	b = append(b, '[')
	for i, id := range page {
		if i > 0 {
			b = append(b, ',')
		}
		c := parseCommit(r.objs[id].data)
		msg := strings.TrimSuffix(c.message, "\n")
		b = append(b, `{"sha":`...)
		b = appendJSONString(b, id.hex())
		b = append(b, `,"commit":{"message":`...)
		b = appendJSONString(b, msg)
		b = append(b, `,"author":{"name":`...)
		b = appendJSONString(b, viewer.name)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, viewer.email)
		b = append(b, `,"date":`...)
		b = appendJSONString(b, trialISO)
		b = append(b, `},"committer":{"name":`...)
		b = appendJSONString(b, viewer.name)
		b = append(b, `,"email":`...)
		b = appendJSONString(b, viewer.email)
		b = append(b, `,"date":`...)
		b = appendJSONString(b, trialISO)
		b = append(b, `}},"author":`...)
		b = append(b, userJSON(viewer)...)
		b = append(b, `}`...)
	}
	b = append(b, ']')
	return &response{status: 200, body: b, extra: append(oauthScopes(), extra...)}
}

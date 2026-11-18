// GraphQL: a small recursive-descent parser for the documents the suite
// sends, plus an executor over the same store the REST API uses.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// --- parser ---------------------------------------------------------------

type gqlField struct {
	alias  string
	name   string
	args   map[string]any
	sel    []*gqlField
	frag   string // type condition for inline fragments
	isFrag bool
}

type gqlVar struct{ name string }

type gqlParser struct {
	s string
	i int
}

func (p *gqlParser) skipWS() {
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.i++
		} else if c == '#' {
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		} else {
			return
		}
	}
}

func (p *gqlParser) peek() byte {
	p.skipWS()
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *gqlParser) name() (string, bool) {
	p.skipWS()
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.i++
		} else {
			break
		}
	}
	if p.i == start {
		return "", false
	}
	return p.s[start:p.i], true
}

func (p *gqlParser) expect(c byte) bool {
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

// parseDocument parses one operation; returns its selection set.
func parseDocument(s string) ([]*gqlField, bool) {
	p := &gqlParser{s: s}
	// Optional operation keyword and name.
	if n, ok := p.name(); ok {
		if n == "query" || n == "mutation" || n == "subscription" {
			// Optional operation name.
			p.name()
			// Optional variable definitions: skip balanced parens.
			if p.peek() == '(' {
				depth := 0
				for p.i < len(p.s) {
					c := p.s[p.i]
					p.i++
					if c == '(' {
						depth++
					} else if c == ')' {
						depth--
						if depth == 0 {
							break
						}
					} else if c == '"' {
						// Skip string contents.
						for p.i < len(p.s) && p.s[p.i] != '"' {
							if p.s[p.i] == '\\' {
								p.i++
							}
							p.i++
						}
						if p.i < len(p.s) {
							p.i++
						}
					}
				}
			}
		} else {
			// Bare selection set starting with a name: rewind.
			p.i = 0
		}
	}
	sel, ok := p.selectionSet()
	if !ok {
		return nil, false
	}
	return sel, true
}

func (p *gqlParser) selectionSet() ([]*gqlField, bool) {
	if !p.expect('{') {
		return nil, false
	}
	var fields []*gqlField
	for {
		if p.peek() == '}' {
			p.i++
			return fields, true
		}
		f, ok := p.selection()
		if !ok {
			return nil, false
		}
		if f != nil {
			fields = append(fields, f)
		}
	}
}

func (p *gqlParser) selection() (*gqlField, bool) {
	// Inline fragment or named spread.
	if p.peek() == '.' {
		if !strings.HasPrefix(p.s[p.i:], "...") {
			return nil, false
		}
		p.i += 3
		p.skipWS()
		f := &gqlField{isFrag: true}
		if n, ok := p.name(); ok {
			if n == "on" {
				cond, ok := p.name()
				if !ok {
					return nil, false
				}
				f.frag = cond
			} else {
				// Named fragment spread: not used by the suite; ignore.
				return nil, true
			}
		}
		sel, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
		return f, true
	}
	name, ok := p.name()
	if !ok {
		return nil, false
	}
	f := &gqlField{name: name, alias: name}
	// Alias?
	if p.peek() == ':' {
		p.i++
		real, ok := p.name()
		if !ok {
			return nil, false
		}
		f.name = real
	}
	// Arguments?
	if p.peek() == '(' {
		args, ok := p.arguments()
		if !ok {
			return nil, false
		}
		f.args = args
	}
	// Sub-selection?
	if p.peek() == '{' {
		sel, ok := p.selectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
	}
	return f, true
}

func (p *gqlParser) arguments() (map[string]any, bool) {
	if !p.expect('(') {
		return nil, false
	}
	args := make(map[string]any)
	for {
		if p.peek() == ')' {
			p.i++
			return args, true
		}
		name, ok := p.name()
		if !ok {
			return nil, false
		}
		if !p.expect(':') {
			return nil, false
		}
		v, ok := p.value()
		if !ok {
			return nil, false
		}
		args[name] = v
	}
}

func (p *gqlParser) value() (any, bool) {
	p.skipWS()
	if p.i >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		s, ok := p.gqlString()
		if !ok {
			return nil, false
		}
		return s, true
	case c == '$':
		p.i++
		n, ok := p.name()
		if !ok {
			return nil, false
		}
		return gqlVar{n}, true
	case c == '[':
		p.i++
		var arr []any
		for {
			if p.peek() == ']' {
				p.i++
				return arr, true
			}
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			arr = append(arr, v)
		}
	case c == '{':
		p.i++
		obj := make(map[string]any)
		for {
			if p.peek() == '}' {
				p.i++
				return obj, true
			}
			n, ok := p.name()
			if !ok {
				return nil, false
			}
			if !p.expect(':') {
				return nil, false
			}
			v, ok := p.value()
			if !ok {
				return nil, false
			}
			obj[n] = v
		}
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.i
		if p.s[p.i] == '-' {
			p.i++
		}
		for p.i < len(p.s) && ((p.s[p.i] >= '0' && p.s[p.i] <= '9') || p.s[p.i] == '.' || p.s[p.i] == 'e' || p.s[p.i] == 'E' || p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		num := p.s[start:p.i]
		if strings.ContainsAny(num, ".eE") {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return nil, false
			}
			return f, true
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return nil, false
		}
		return n, true
	default:
		n, ok := p.name()
		if !ok {
			return nil, false
		}
		switch n {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null":
			return nil, true
		default:
			return n, true // enum value
		}
	}
}

func (p *gqlParser) gqlString() (string, bool) {
	if !p.expect('"') {
		return "", false
	}
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			p.i++
			return b.String(), true
		}
		if c == '\\' {
			p.i++
			if p.i >= len(p.s) {
				return "", false
			}
			switch e := p.s[p.i]; e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.i+4 >= len(p.s) {
					return "", false
				}
				r := parseHex4([]byte(p.s[p.i+1 : p.i+5]))
				b.WriteRune(r)
				p.i += 4
			default:
				return "", false
			}
			p.i++
			continue
		}
		b.WriteByte(c)
		p.i++
	}
	return "", false
}

// --- executor -------------------------------------------------------------

type gqlCtx struct {
	vars map[string]any
	errs []string
}

// resolveVars converts a jval variables object to plain Go values.
func varsFromJval(v *jval) map[string]any {
	out := make(map[string]any)
	if v == nil || v.kind != 'o' {
		return out
	}
	for k, val := range v.obj {
		out[k] = jvalToAny(val)
	}
	return out
}

func jvalToAny(v *jval) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case 's':
		return v.str
	case 'n':
		if v.num == float64(int(v.num)) {
			return int(v.num)
		}
		return v.num
	case 'b':
		return v.num != 0
	case 'a':
		arr := make([]any, len(v.arr))
		for i, e := range v.arr {
			arr[i] = jvalToAny(e)
		}
		return arr
	case 'o':
		m := make(map[string]any)
		for k, e := range v.obj {
			m[k] = jvalToAny(e)
		}
		return m
	}
	return nil
}

// argVal resolves an argument value: variables are substituted.
func (c *gqlCtx) argVal(v any) any {
	if vr, ok := v.(gqlVar); ok {
		return c.vars[vr.name]
	}
	return v
}

func (c *gqlCtx) argStr(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := c.argVal(v).(string); ok {
			return s
		}
	}
	return ""
}

func (c *gqlCtx) argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := c.argVal(v).(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return def
}

func (c *gqlCtx) argObj(args map[string]any, key string) map[string]any {
	if v, ok := args[key]; ok {
		if m, ok := c.argVal(v).(map[string]any); ok {
			return m
		}
	}
	return nil
}

// execute runs one document and returns the response object.
func executeGQL(doc string, vars map[string]any) map[string]any {
	sel, ok := parseDocument(doc)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "syntax error"}}}
	}
	ctx := &gqlCtx{vars: vars}
	data := execSel(sel, "Query", nil, ctx)
	resp := map[string]any{"data": data}
	if len(ctx.errs) > 0 {
		errs := make([]any, len(ctx.errs))
		for i, e := range ctx.errs {
			errs[i] = map[string]any{"message": e}
		}
		resp["errors"] = errs
	}
	return resp
}

// execSel executes a selection set against obj of the given GraphQL type.
// Returns a map of alias -> value.
func execSel(sel []*gqlField, typ string, obj any, ctx *gqlCtx) map[string]any {
	out := make(map[string]any)
	for _, f := range sel {
		if f.isFrag {
			if f.frag != "" && f.frag != typ {
				continue
			}
			sub := execSel(f.sel, typ, obj, ctx)
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		val, ok := resolveField(typ, obj, f, ctx)
		if !ok {
			continue // error already recorded; field omitted
		}
		out[f.alias] = val
	}
	return out
}

// resolveField resolves one field. ok=false means the field is unknown
// (error recorded) and must be omitted from the parent object.
func resolveField(typ string, obj any, f *gqlField, ctx *gqlCtx) (any, bool) {
	if f.name == "__typename" {
		return typ, true
	}
	// Mutation payloads are plain maps built at mutation time.
	if m, ok := obj.(map[string]any); ok {
		v, exists := m[f.name]
		if !exists {
			ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "`+typ+`".`)
			return nil, false
		}
		if v == nil {
			return nil, true
		}
		switch f.name {
		case "issue":
			return execSel(f.sel, "Issue", v, ctx), true
		case "pullRequest":
			return execSel(f.sel, "PullRequest", v, ctx), true
		case "commentEdge":
			return execSel(f.sel, "CommentEdge", v, ctx), true
		}
		return v, true
	}
	switch typ {
	case "Query":
		return resolveQuery(f, ctx)
	case "User":
		if u, ok := obj.(*user); ok && f.name == "login" {
			return u.login, true
		}
	case "Repository":
		return resolveRepository(obj.(*Repo), f, ctx)
	case "Issue":
		return resolveIssue(obj.(*issue), f, ctx)
	case "PullRequest":
		return resolvePull(obj.(*pull), f, ctx)
	case "IssueConnection":
		return resolveIssueConn(obj.(*issueConn), f, ctx)
	case "PullRequestConnection":
		return resolvePullConn(obj.(*pullConn), f, ctx)
	case "FileConnection":
		return resolveFileConn(obj.(*fileConn), f, ctx)
	case "PageInfo":
		if p, ok := obj.(*pageInfo); ok {
			switch f.name {
			case "hasNextPage":
				return p.hasNext, true
			case "endCursor":
				if p.endCursor == "" {
					return nil, true
				}
				return p.endCursor, true
			}
		}
	case "BranchRef":
		if b, ok := obj.(*branchRef); ok && f.name == "name" {
			return b.name, true
		}
	case "IssueEdge":
		if e, ok := obj.(*issueEdge); ok && f.name == "node" {
			return execSel(f.sel, "Issue", e.node, ctx), true
		}
	case "PullRequestEdge":
		if e, ok := obj.(*pullEdge); ok && f.name == "node" {
			return execSel(f.sel, "PullRequest", e.node, ctx), true
		}
	case "FileEdge":
		if e, ok := obj.(*fileEdge); ok && f.name == "node" {
			return execSel(f.sel, "File", e.node, ctx), true
		}
	case "File":
		if fl, ok := obj.(*pullFile); ok && f.name == "path" {
			return fl.name, true
		}
	case "__Type":
		return resolveIntrospection(obj.(*gqlType), f, ctx)
	case "CommentEdge":
		if e, ok := obj.(*commentEdge); ok && f.name == "node" {
			return execSel(f.sel, "IssueComment", e.node, ctx), true
		}
	case "IssueComment":
		if cm, ok := obj.(*comment); ok {
			switch f.name {
			case "id":
				return cm.gid, true
			case "body":
				return cm.body, true
			}
		}
	}
	// Unknown field on this type.
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "`+typ+`".`)
	return nil, false
}

// --- root fields ----------------------------------------------------------

func resolveQuery(f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "viewer":
		u := viewer
		return execSel(f.sel, "User", &u, ctx), true
	case "repository":
		owner := ctx.argStr(f.args, "owner")
		name := ctx.argStr(f.args, "name")
		if owner != viewer.login {
			return nil, true
		}
		r := gstore.getRepo(name)
		if r == nil {
			return nil, true
		}
		return execSel(f.sel, "Repository", r, ctx), true
	case "node":
		id := ctx.argStr(f.args, "id")
		kind, nums, ok := decodeGID(id)
		if !ok {
			return nil, true
		}
		switch kind {
		case 'R':
			if len(nums) < 1 {
				return nil, true
			}
			for _, r := range gstore.order {
				if r.id == nums[0] {
					return execSel(f.sel, "Repository", r, ctx), true
				}
			}
			return nil, true
		case 'I':
			if len(nums) < 2 {
				return nil, true
			}
			for _, r := range gstore.order {
				if r.id == nums[0] {
					if is := r.issueByNum[nums[1]]; is != nil {
						return execSel(f.sel, "Issue", is, ctx), true
					}
				}
			}
			return nil, true
		case 'P':
			if len(nums) < 2 {
				return nil, true
			}
			for _, r := range gstore.order {
				if r.id == nums[0] {
					if p := r.pullByNum[nums[1]]; p != nil {
						return execSel(f.sel, "PullRequest", p, ctx), true
					}
				}
			}
			return nil, true
		}
		return nil, true
	case "__type":
		name := ctx.argStr(f.args, "name")
		t, ok := gqlTypes[name]
		if !ok {
			return nil, true
		}
		return execSel(f.sel, "__Type", t, ctx), true
	case "createIssue":
		return mutateCreateIssue(f, ctx)
	case "closeIssue":
		return mutateCloseIssue(f, ctx)
	case "createPullRequest":
		return mutateCreatePullRequest(f, ctx)
	case "mergePullRequest":
		return mutateMergePullRequest(f, ctx)
	case "addComment":
		return mutateAddComment(f, ctx)
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "Query".`)
	return nil, false
}

// --- mutations ------------------------------------------------------------

func findRepoByGID(gid string) *Repo {
	kind, nums, ok := decodeGID(gid)
	if !ok || kind != 'R' || len(nums) < 1 {
		return nil
	}
	for _, r := range gstore.order {
		if r.id == nums[0] {
			return r
		}
	}
	return nil
}

func mutateCreateIssue(f *gqlField, ctx *gqlCtx) (any, bool) {
	input := ctx.argObj(f.args, "input")
	if input == nil {
		ctx.errs = append(ctx.errs, `Variable $input of required type CreateIssueInput! was not provided.`)
		return nil, false
	}
	repoID, _ := input["repositoryId"].(string)
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	r := findRepoByGID(repoID)
	if r == nil {
		return map[string]any{"issue": nil}, true
	}
	is := &issue{
		num:    r.nextIssue,
		id:     gstore.nextIssueID,
		title:  title,
		body:   body,
		state:  "open",
		seq:    len(r.issues),
	}
	is.gid = gidIssue(r.id, is.num)
	gstore.nextIssueID++
	r.nextIssue++
	r.issues = append(r.issues, is)
	r.issueByNum[is.num] = is
	is.renderJSON(r)
	return execSel(f.sel, "CreateIssuePayload", map[string]any{"issue": is}, ctx), true
}

func mutateCloseIssue(f *gqlField, ctx *gqlCtx) (any, bool) {
	input := ctx.argObj(f.args, "input")
	if input == nil {
		return nil, false
	}
	issueID, _ := input["issueId"].(string)
	kind, nums, ok := decodeGID(issueID)
	if !ok || kind != 'I' || len(nums) < 2 {
		return map[string]any{"issue": nil}, true
	}
	var is *issue
	for _, r := range gstore.order {
		if r.id == nums[0] {
			is = r.issueByNum[nums[1]]
			break
		}
	}
	if is == nil {
		return map[string]any{"issue": nil}, true
	}
	is.state = "closed"
	// Re-render with the owning repo.
	for _, r := range gstore.order {
		if r.id == nums[0] {
			is.renderJSON(r)
			break
		}
	}
	return execSel(f.sel, "CloseIssuePayload", map[string]any{"issue": is}, ctx), true
}

func mutateCreatePullRequest(f *gqlField, ctx *gqlCtx) (any, bool) {
	input := ctx.argObj(f.args, "input")
	if input == nil {
		return nil, false
	}
	repoID, _ := input["repositoryId"].(string)
	base, _ := input["baseRefName"].(string)
	head, _ := input["headRefName"].(string)
	title, _ := input["title"].(string)
	r := findRepoByGID(repoID)
	if r == nil {
		return map[string]any{"pullRequest": nil}, true
	}
	headTip, ok1 := r.refs["refs/heads/"+head]
	baseTip, ok2 := r.refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return map[string]any{"pullRequest": nil}, true
	}
	p := &pull{
		num:     r.nextPull,
		id:      gstore.nextPullID,
		title:   title,
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
	return execSel(f.sel, "CreatePullRequestPayload", map[string]any{"pullRequest": p}, ctx), true
}

func mutateMergePullRequest(f *gqlField, ctx *gqlCtx) (any, bool) {
	input := ctx.argObj(f.args, "input")
	if input == nil {
		return nil, false
	}
	prID, _ := input["pullRequestId"].(string)
	kind, nums, ok := decodeGID(prID)
	if !ok || kind != 'P' || len(nums) < 2 {
		return map[string]any{"pullRequest": nil}, true
	}
	var p *pull
	var r *Repo
	for _, repo := range gstore.order {
		if repo.id == nums[0] {
			r = repo
			p = repo.pullByNum[nums[1]]
			break
		}
	}
	if p == nil || r == nil {
		return map[string]any{"pullRequest": nil}, true
	}
	if p.state == "open" {
		baseTip, ok1 := r.refs["refs/heads/"+p.baseRef]
		headTip, ok2 := r.refs["refs/heads/"+p.headRef]
		if ok1 && ok2 {
			if mergedTree, conflict := r.threeWayMerge(baseTip, headTip); !conflict {
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
			}
		}
	}
	return execSel(f.sel, "MergePullRequestPayload", map[string]any{"pullRequest": p}, ctx), true
}

func mutateAddComment(f *gqlField, ctx *gqlCtx) (any, bool) {
	input := ctx.argObj(f.args, "input")
	if input == nil {
		return nil, false
	}
	subjectID, _ := input["subjectId"].(string)
	body, _ := input["body"].(string)
	kind, nums, ok := decodeGID(subjectID)
	if !ok || kind != 'I' || len(nums) < 2 {
		return map[string]any{"commentEdge": nil}, true
	}
	var is *issue
	var r *Repo
	for _, repo := range gstore.order {
		if repo.id == nums[0] {
			r = repo
			is = repo.issueByNum[nums[1]]
			break
		}
	}
	if is == nil || r == nil {
		return map[string]any{"commentEdge": nil}, true
	}
	c := &comment{id: gstore.nextCommentID, body: body, seq: len(is.comments)}
	c.gid = gidComment(r.id, is.num, c.id)
	gstore.nextCommentID++
	c.renderJSON(r, is.num)
	is.comments = append(is.comments, c)
	is.renderJSON(r)
	edge := &commentEdge{node: c}
	return execSel(f.sel, "AddCommentPayload", map[string]any{"commentEdge": edge}, ctx), true
}

// --- type resolvers -------------------------------------------------------

type branchRef struct{ name string }

type pageInfo struct {
	hasNext   bool
	endCursor string
}

type issueConn struct {
	items   []*issue
	hasNext bool
	end     string
}

type pullConn struct {
	items []*pull
}

type fileConn struct {
	files []pullFile
}

type issueEdge struct{ node *issue }
type pullEdge struct{ node *pull }
type fileEdge struct{ node *pullFile }
type commentEdge struct{ node *comment }

func resolveRepository(r *Repo, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "id":
		return gidRepo(r.id), true
	case "databaseId":
		return r.id, true
	case "name":
		return r.name, true
	case "nameWithOwner":
		return viewer.login + "/" + r.name, true
	case "owner":
		u := viewer
		return execSel(f.sel, "User", &u, ctx), true
	case "defaultBranchRef":
		if _, ok := r.defaultTip(); !ok {
			return nil, true
		}
		return execSel(f.sel, "BranchRef", &branchRef{name: r.defaultBranch}, ctx), true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "issue":
		num := ctx.argInt(f.args, "number", 0)
		is := r.issueByNum[num]
		if is == nil {
			return nil, true
		}
		return execSel(f.sel, "Issue", is, ctx), true
	case "issues":
		return execSel(f.sel, "IssueConnection", buildIssueConn(r, f, ctx), ctx), true
	case "pullRequests":
		return execSel(f.sel, "PullRequestConnection", &pullConn{items: r.pulls}, ctx), true
	case "pullRequest":
		num := ctx.argInt(f.args, "number", 0)
		p := r.pullByNum[num]
		if p == nil {
			return nil, true
		}
		return execSel(f.sel, "PullRequest", p, ctx), true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "Repository".`)
	return nil, false
}

// buildIssueConn applies first/states/orderBy/after to the repo's issues.
func buildIssueConn(r *Repo, f *gqlField, ctx *gqlCtx) *issueConn {
	first := ctx.argInt(f.args, "first", 0)
	states := map[string]bool{}
	if v, ok := f.args["states"]; ok {
		if arr, ok := ctx.argVal(v).([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states[str] = true
				}
			}
		}
	}
	// orderBy: {field: CREATED_AT, direction: DESC|ASC}
	desc := false
	if ob := ctx.argObj(f.args, "orderBy"); ob != nil {
		if d, ok := ob["direction"].(string); ok && d == "DESC" {
			desc = true
		}
	}
	after := ctx.argStr(f.args, "after")
	afterNum := 0
	if after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			if n, err := strconv.Atoi(string(raw)); err == nil {
				afterNum = n
			}
		}
	}
	var items []*issue
	for _, is := range r.issues {
		st := "OPEN"
		if is.state == "closed" {
			st = "CLOSED"
		}
		if len(states) > 0 && !states[st] {
			continue
		}
		if afterNum > 0 && is.num <= afterNum {
			continue
		}
		items = append(items, is)
	}
	if desc {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	hasNext := false
	end := ""
	if first > 0 && len(items) > first {
		hasNext = true
		items = items[:first]
	}
	if len(items) > 0 {
		end = base64.StdEncoding.EncodeToString([]byte(itoa(items[len(items)-1].num)))
	}
	return &issueConn{items: items, hasNext: hasNext, end: end}
}

func resolveIssue(is *issue, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "id":
		return is.gid, true
	case "number":
		return is.num, true
	case "title":
		return is.title, true
	case "state":
		if is.state == "open" {
			return "OPEN", true
		}
		return "CLOSED", true
	case "body":
		return is.body, true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "Issue".`)
	return nil, false
}

func resolvePull(p *pull, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "id":
		return p.gid, true
	case "number":
		return p.num, true
	case "headRefName":
		return p.headRef, true
	case "baseRefName":
		return p.baseRef, true
	case "mergeable":
		if p.mergeable {
			return "MERGEABLE", true
		}
		return "CONFLICTING", true
	case "state":
		if p.state == "open" {
			return "OPEN", true
		}
		return "CLOSED", true
	case "merged":
		return p.merged, true
	case "files":
		first := ctx.argInt(f.args, "first", 0)
		files := p.files
		if first > 0 && len(files) > first {
			files = files[:first]
		}
		return execSel(f.sel, "FileConnection", &fileConn{files: files}, ctx), true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "PullRequest".`)
	return nil, false
}

func resolveIssueConn(c *issueConn, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "nodes":
		nodes := make([]any, len(c.items))
		for i, is := range c.items {
			nodes[i] = execSel(f.sel, "Issue", is, ctx)
		}
		return nodes, true
	case "edges":
		edges := make([]any, len(c.items))
		for i, is := range c.items {
			edges[i] = execSel(f.sel, "IssueEdge", &issueEdge{node: is}, ctx)
		}
		return edges, true
	case "pageInfo":
		return execSel(f.sel, "PageInfo", &pageInfo{hasNext: c.hasNext, endCursor: c.end}, ctx), true
	case "totalCount":
		return len(c.items), true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "IssueConnection".`)
	return nil, false
}

func resolvePullConn(c *pullConn, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "nodes":
		nodes := make([]any, len(c.items))
		for i, p := range c.items {
			nodes[i] = execSel(f.sel, "PullRequest", p, ctx)
		}
		return nodes, true
	case "edges":
		edges := make([]any, len(c.items))
		for i, p := range c.items {
			edges[i] = execSel(f.sel, "PullRequestEdge", &pullEdge{node: p}, ctx)
		}
		return edges, true
	case "pageInfo":
		return execSel(f.sel, "PageInfo", &pageInfo{}, ctx), true
	case "totalCount":
		return len(c.items), true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "PullRequestConnection".`)
	return nil, false
}

func resolveFileConn(c *fileConn, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "nodes":
		nodes := make([]any, len(c.files))
		for i := range c.files {
			nodes[i] = execSel(f.sel, "File", &c.files[i], ctx)
		}
		return nodes, true
	case "edges":
		edges := make([]any, len(c.files))
		for i := range c.files {
			edges[i] = execSel(f.sel, "FileEdge", &fileEdge{node: &c.files[i]}, ctx)
		}
		return edges, true
	case "pageInfo":
		return execSel(f.sel, "PageInfo", &pageInfo{}, ctx), true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "FileConnection".`)
	return nil, false
}

// --- introspection ----------------------------------------------------------

type gqlType struct {
	name       string
	fields     []string
	enumValues []string
}

var gqlTypes = map[string]*gqlType{
	"Issue": {
		name:   "Issue",
		fields: []string{"id", "number", "title", "state", "body"},
	},
	"PullRequest": {
		name:   "PullRequest",
		fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	},
	"Repository": {
		name:   "Repository",
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	},
	"SearchType": {
		name:       "SearchType",
		enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "MILESTONE", "ORGANIZATION", "COMMIT"},
	},
	"User": {
		name:   "User",
		fields: []string{"id", "login", "name", "email"},
	},
}

func resolveIntrospection(t *gqlType, f *gqlField, ctx *gqlCtx) (any, bool) {
	switch f.name {
	case "name":
		return t.name, true
	case "fields":
		fields := make([]any, len(t.fields))
		for i, name := range t.fields {
			fields[i] = map[string]any{"name": name}
		}
		return fields, true
	case "enumValues":
		vals := make([]any, len(t.enumValues))
		for i, name := range t.enumValues {
			vals[i] = map[string]any{"name": name}
		}
		return vals, true
	}
	ctx.errs = append(ctx.errs, `Cannot query field "`+f.name+`" on type "__Type".`)
	return nil, false
}

// --- HTTP handler ---------------------------------------------------------

// handleGraphQL answers POST /api/graphql (single or batched documents).
func handleGraphQL(req *request) *response {
	if req.method != "POST" {
		return &response{status: 405, body: jsonError("method not allowed"), extra: oauthScopes()}
	}
	body, ok := parseJSON(req.body)
	if !ok {
		return &response{status: 400, body: jsonError("Problems parsing JSON"), extra: oauthScopes()}
	}
	storeMu()
	defer storeUnlock()
	out := make([]byte, 0, 512)
	if body.kind == 'a' {
		out = append(out, '[')
		for i, el := range body.arr {
			if i > 0 {
				out = append(out, ',')
			}
			out = appendGraphQLResult(out, el)
		}
		out = append(out, ']')
	} else {
		out = appendGraphQLResult(out, body)
	}
	return &response{status: 200, body: out, extra: oauthScopes()}
}

func appendGraphQLResult(dst []byte, el *jval) []byte {
	query := el.get("query").strOr("")
	vars := varsFromJval(el.get("variables"))
	res := executeGQL(query, vars)
	return appendJSONValue(dst, res)
}

// appendJSONValue renders a plain Go value (map/slice/string/int/bool/nil).
func appendJSONValue(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case string:
		return appendJSONString(dst, t)
	case int:
		return appendInt(dst, t)
	case bool:
		return appendBool(dst, t)
	case []any:
		dst = append(dst, '[')
		for i, e := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONValue(dst, e)
		}
		return append(dst, ']')
	case map[string]any:
		dst = append(dst, '{')
		i := 0
		for k, e := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			i++
			dst = appendJSONString(dst, k)
			dst = append(dst, ':')
			dst = appendJSONValue(dst, e)
		}
		return append(dst, '}')
	default:
		return append(dst, "null"...)
	}
}

// itoa64 renders an int64 without fmt.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

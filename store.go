// In-memory state: repositories, refs, objects, issues, pull requests.
// All mutations go through the store mutex; handlers hold it only around
// the state operation, never across socket I/O.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"sort"
	"sync"
)

type store struct {
	mu sync.Mutex

	repos map[string]*Repo
	order []*Repo

	nextRepoID    int
	nextIssueID   int
	nextCommentID int
	nextPullID    int
}

type label struct {
	name  string
	color string
}

type comment struct {
	id   int
	body string
	seq  int
	gid  string
	json []byte // pre-rendered JSON object
}

type issue struct {
	num      int
	id       int
	gid      string
	title    string
	body     string
	state    string // "open" | "closed"
	labels   []string
	seq      int
	comments []*comment
	json     []byte // pre-rendered JSON object (state included)
}

type pull struct {
	num       int
	id        int
	gid       string
	title     string
	body      string
	headRef   string
	baseRef   string
	headSha   oid
	baseSha   oid
	state     string // "open" | "closed"
	merged    bool
	mergedAt  string
	mergeSha  oid
	mergeable bool
	seq       int
	json      []byte // pre-rendered JSON object
	files     []pullFile
}

type pullFile struct {
	name      string
	status    string // added | modified | removed
	additions int
	deletions int
}

type packKey struct {
	ver uint64
	key [20]byte
}

type Repo struct {
	name          string
	id            int
	desc          string
	priv          bool
	defaultBranch string
	refs          map[string]oid
	objs          map[oid]*obj
	commitDepths  map[oid]int
	labels        map[string]*label
	issues        []*issue
	issueByNum    map[int]*issue
	pulls         []*pull
	pullByNum     map[int]*pull
	nextIssue     int
	nextPull      int
	nextComment   int
	version       uint64
	seq           int

	// Caches, invalidated by bumping version.
	advV2Ver  uint64
	advV2Cache []byte
	advV0Ver  uint64
	advV0Cache []byte
	lsVer     uint64
	lsDefault []byte
	packs     map[packKey][]byte

	json []byte // pre-rendered repo JSON object
}

func newStore() *store {
	return &store{
		repos:       make(map[string]*Repo),
		nextRepoID:  1,
		nextIssueID: 1,
		nextCommentID: 1,
		nextPullID:  1,
	}
}

// bump invalidates every derived cache of the repo.
func (r *Repo) bump() {
	r.version++
	r.packs = make(map[packKey][]byte)
}

// createRepo registers a new repository owned by the viewer.
func (s *store) createRepo(name, desc string, priv bool) *Repo {
	r := &Repo{
		name:          name,
		id:            s.nextRepoID,
		desc:          desc,
		priv:          priv,
		defaultBranch: "main",
		refs:          make(map[string]oid),
		objs:          make(map[oid]*obj),
		commitDepths:  make(map[oid]int),
		labels:        make(map[string]*label),
		issueByNum:    make(map[int]*issue),
		pullByNum:     make(map[int]*pull),
		nextIssue:     1,
		nextPull:      1,
		nextComment:   1,
		seq:           len(s.order),
		packs:         make(map[packKey][]byte),
	}
	s.nextRepoID++
	s.repos[name] = r
	s.order = append(s.order, r)
	r.renderRepoJSON()
	return r
}

func (s *store) getRepo(name string) *Repo { return s.repos[name] }

// deleteRepo removes a repo from the store.
func (s *store) deleteRepo(name string) {
	delete(s.repos, name)
	for i, r := range s.order {
		if r.name == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// defaultTip returns the oid of the default branch, if any.
func (r *Repo) defaultTip() (oid, bool) {
	id, ok := r.refs["refs/heads/"+r.defaultBranch]
	return id, ok
}

// headRef returns the full ref name HEAD points at.
func (r *Repo) headRef() string { return "refs/heads/" + r.defaultBranch }

// sortedRefNames returns all ref names, heads first then tags, sorted.
func (r *Repo) sortedRefNames() []string {
	heads := []string{}
	tags := []string{}
	other := []string{}
	for name := range r.refs {
		switch {
		case len(name) > 11 && name[:11] == "refs/heads/":
			heads = append(heads, name)
		case len(name) > 10 && name[:10] == "refs/tags/":
			tags = append(tags, name)
		default:
			other = append(other, name)
		}
	}
	sort.Strings(heads)
	sort.Strings(tags)
	sort.Strings(other)
	return append(append(heads, tags...), other...)
}

// --- global ids (opaque, stable) ------------------------------------------

func gidRepo(id int) string {
	return base64.StdEncoding.EncodeToString([]byte("R:" + itoa(id)))
}

func gidIssue(repoID, num int) string {
	return base64.StdEncoding.EncodeToString([]byte("I:" + itoa(repoID) + ":" + itoa(num)))
}

func gidPull(repoID, num int) string {
	return base64.StdEncoding.EncodeToString([]byte("P:" + itoa(repoID) + ":" + itoa(num)))
}

func gidComment(repoID, issueNum, cid int) string {
	return base64.StdEncoding.EncodeToString([]byte("C:" + itoa(repoID) + ":" + itoa(issueNum) + ":" + itoa(cid)))
}

// decodeGID returns the kind ("R", "I", "P", "C") and the numeric parts.
func decodeGID(g string) (byte, []int, bool) {
	raw, err := base64.StdEncoding.DecodeString(g)
	if err != nil || len(raw) < 3 {
		return 0, nil, false
	}
	parts := bytes.Split(raw, []byte(":"))
	if len(parts) < 2 {
		return 0, nil, false
	}
	kind := parts[0][0]
	if kind != 'R' && kind != 'I' && kind != 'P' && kind != 'C' {
		return 0, nil, false
	}
	nums := make([]int, 0, len(parts)-1)
	for _, p := range parts[1:] {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, nil, false
			}
			n = n*10 + int(c-'0')
		}
		nums = append(nums, n)
	}
	return kind, nums, true
}

// --- JSON rendering (pre-rendered, cached) --------------------------------

func userJSON(u user) []byte {
	b := make([]byte, 0, 64)
	b = append(b, `{"login":`...)
	b = appendJSONString(b, u.login)
	b = append(b, `,"id":`...)
	b = appendInt(b, u.id)
	b = append(b, `,"type":"User"}`...)
	return b
}

func (r *Repo) renderRepoJSON() {
	b := make([]byte, 0, 256)
	b = append(b, `{"id":`...)
	b = appendInt(b, r.id)
	b = append(b, `,"name":`...)
	b = appendJSONString(b, r.name)
	b = append(b, `,"full_name":`...)
	b = appendJSONString(b, viewer.login+"/"+r.name)
	b = append(b, `,"owner":`...)
	b = append(b, userJSON(viewer)...)
	b = append(b, `,"private":`...)
	b = appendBool(b, r.priv)
	b = append(b, `,"description":`...)
	if r.desc == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, r.desc)
	}
	b = append(b, `,"default_branch":`...)
	b = appendJSONString(b, r.defaultBranch)
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://github.com/"+viewer.login+"/"+r.name)
	b = append(b, `,"clone_url":`...)
	b = appendJSONString(b, "https://github.com/"+viewer.login+"/"+r.name+".git")
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"has_issues":true,"fork":false,"archived":false,"disabled":false,"visibility":`...)
	if r.priv {
		b = appendJSONString(b, "private")
	} else {
		b = appendJSONString(b, "public")
	}
	b = append(b, `,"forks_count":0,"stargazers_count":0,"watchers_count":0,"open_issues_count":`...)
	b = appendInt(b, r.openIssueCount())
	b = append(b, `,"license":null,"topics":[]}`...)
	r.json = b
}

func (r *Repo) openIssueCount() int {
	n := 0
	for _, is := range r.issues {
		if is.state == "open" {
			n++
		}
	}
	return n
}

func (r *Repo) labelJSON(name string) []byte {
	l, ok := r.labels[name]
	color := "d73a4a"
	if ok {
		color = l.color
	}
	b := make([]byte, 0, 48)
	b = append(b, `{"name":`...)
	b = appendJSONString(b, name)
	b = append(b, `,"color":`...)
	b = appendJSONString(b, color)
	b = append(b, `,"description":""}`...)
	return b
}

func (is *issue) renderJSON(r *Repo) {
	b := make([]byte, 0, 256)
	b = append(b, `{"id":`...)
	b = appendInt(b, is.id)
	b = append(b, `,"number":`...)
	b = appendInt(b, is.num)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, is.title)
	b = append(b, `,"body":`...)
	if is.body == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, is.body)
	}
	b = append(b, `,"state":`...)
	b = appendJSONString(b, is.state)
	b = append(b, `,"labels":[`...)
	for i, name := range is.labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, r.labelJSON(name)...)
	}
	b = append(b, `],"user":`...)
	b = append(b, userJSON(viewer)...)
	b = append(b, `,"assignee":null,"created_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"comments":`...)
	b = appendInt(b, len(is.comments))
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://github.com/"+viewer.login+"/"+r.name+"/issues/"+itoa(is.num))
	b = append(b, `}`...)
	is.json = b
}

func (c *comment) renderJSON(r *Repo, issueNum int) {
	b := make([]byte, 0, 128)
	b = append(b, `{"id":`...)
	b = appendInt(b, c.id)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, c.body)
	b = append(b, `,"user":`...)
	b = append(b, userJSON(viewer)...)
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://github.com/"+viewer.login+"/"+r.name+"/issues/"+itoa(issueNum)+"#issuecomment-"+itoa(c.id))
	b = append(b, `}`...)
	c.json = b
}

func (p *pull) renderJSON(r *Repo) {
	b := make([]byte, 0, 320)
	b = append(b, `{"id":`...)
	b = appendInt(b, p.id)
	b = append(b, `,"number":`...)
	b = appendInt(b, p.num)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, p.title)
	b = append(b, `,"body":`...)
	if p.body == "" {
		b = append(b, "null"...)
	} else {
		b = appendJSONString(b, p.body)
	}
	b = append(b, `,"state":`...)
	b = appendJSONString(b, p.state)
	b = append(b, `,"merged":`...)
	b = appendBool(b, p.merged)
	b = append(b, `,"mergeable":`...)
	b = appendBool(b, p.mergeable)
	if p.merged {
		b = append(b, `,"merged_at":`...)
		b = appendJSONString(b, p.mergedAt)
		b = append(b, `,"merge_commit_sha":`...)
		b = appendJSONString(b, p.mergeSha.hex())
	} else {
		b = append(b, `,"merged_at":null,"merge_commit_sha":null`...)
	}
	b = append(b, `,"head":{"ref":`...)
	b = appendJSONString(b, p.headRef)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, p.headSha.hex())
	b = append(b, `,"label":`...)
	b = appendJSONString(b, viewer.login+":"+p.headRef)
	b = append(b, `},"base":{"ref":`...)
	b = appendJSONString(b, p.baseRef)
	b = append(b, `,"sha":`...)
	b = appendJSONString(b, p.baseSha.hex())
	b = append(b, `,"label":`...)
	b = appendJSONString(b, viewer.login+":"+p.baseRef)
	b = append(b, `},"user":`...)
	b = append(b, userJSON(viewer)...)
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, trialISO)
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://github.com/"+viewer.login+"/"+r.name+"/pull/"+itoa(p.num))
	b = append(b, `}`...)
	p.json = b
}

// --- tree helpers for contents / merges -----------------------------------

// setFile returns the id of a new tree equal to base with path set to
// (mode, blobID). Intermediate trees are created as needed.
func (r *Repo) setFile(base oid, path string, mode string, blobID oid) oid {
	parts := splitPath(path)
	return r.setFileRec(base, parts, mode, blobID)
}

func (r *Repo) setFileRec(base oid, parts []string, mode string, blobID oid) oid {
	var ents []treeEnt
	if base != (oid{}) {
		if o := r.objs[base]; o != nil && o.typ == objTree {
			ents = parseTree(o.data)
		}
	}
	name := parts[0]
	if len(parts) == 1 {
		// Leaf: replace or add.
		found := false
		for i := range ents {
			if ents[i].name == name {
				ents[i] = treeEnt{mode: mode, name: name, id: blobID}
				found = true
				break
			}
		}
		if !found {
			ents = append(ents, treeEnt{mode: mode, name: name, id: blobID})
		}
	} else {
		// Directory: recurse.
		var sub oid
		found := false
		for i := range ents {
			if ents[i].name == name && ents[i].mode == "40000" {
				sub = ents[i].id
				found = true
				break
			}
		}
		sub = r.setFileRec(sub, parts[1:], mode, blobID)
		if found {
			for i := range ents {
				if ents[i].name == name {
					ents[i].id = sub
					break
				}
			}
		} else {
			ents = append(ents, treeEnt{mode: "40000", name: name, id: sub})
		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].name < ents[j].name })
	data := buildTree(ents)
	id := hashObject(objTree, data)
	r.objs[id] = &obj{typ: objTree, data: data}
	return id
}

// deleteFile returns the id of a new tree equal to base with path removed.
func (r *Repo) deleteFile(base oid, path string) (oid, bool) {
	parts := splitPath(path)
	return r.deleteFileRec(base, parts)
}

func (r *Repo) deleteFileRec(base oid, parts []string) (oid, bool) {
	o := r.objs[base]
	if o == nil || o.typ != objTree {
		return oid{}, false
	}
	ents := parseTree(o.data)
	name := parts[0]
	if len(parts) == 1 {
		for i := range ents {
			if ents[i].name == name {
				ents = append(ents[:i], ents[i+1:]...)
				data := buildTree(ents)
				id := hashObject(objTree, data)
				r.objs[id] = &obj{typ: objTree, data: data}
				return id, true
			}
		}
		return oid{}, false
	}
	for i := range ents {
		if ents[i].name == name && ents[i].mode == "40000" {
			sub, ok := r.deleteFileRec(ents[i].id, parts[1:])
			if !ok {
				return oid{}, false
			}
			ents[i].id = sub
			data := buildTree(ents)
			id := hashObject(objTree, data)
			r.objs[id] = &obj{typ: objTree, data: data}
			return id, true
		}
	}
	return oid{}, false
}

// getFile resolves path under tree to (mode, oid).
func (r *Repo) getFile(tree oid, path string) (string, oid, bool) {
	parts := splitPath(path)
	cur := tree
	for i, part := range parts {
		o := r.objs[cur]
		if o == nil || o.typ != objTree {
			return "", oid{}, false
		}
		found := false
		for _, e := range parseTree(o.data) {
			if e.name == part {
				if i == len(parts)-1 {
					return e.mode, e.id, true
				}
				if e.mode != "40000" {
					return "", oid{}, false
				}
				cur = e.id
				found = true
				break
			}
		}
		if !found {
			return "", oid{}, false
		}
	}
	return "", oid{}, false
}

// listDir returns the entries directly under dirPath in tree.
func (r *Repo) listDir(tree oid, dirPath string) ([]treeEnt, bool) {
	cur := tree
	if dirPath != "" {
		_, id, ok := r.getFile(tree, dirPath)
		if !ok {
			return nil, false
		}
		cur = id
	}
	o := r.objs[cur]
	if o == nil || o.typ != objTree {
		return nil, false
	}
	return parseTree(o.data), true
}

func splitPath(p string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				parts = append(parts, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		parts = append(parts, p[start:])
	}
	return parts
}

// --- merge support --------------------------------------------------------

// mergeBase returns the best common ancestor of a and b (first found walking
// a's ancestry that is also an ancestor of b, preferring the newest).
func (r *Repo) mergeBase(a, b oid) (oid, bool) {
	if a == b {
		return a, true
	}
	// Collect b's ancestors.
	bAnc := make(map[oid]bool)
	queue := []oid{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if bAnc[cur] {
			continue
		}
		bAnc[cur] = true
		if o := r.objs[cur]; o != nil && o.typ == objCommit {
			queue = append(queue, parseCommit(o.data).parents...)
		}
	}
	// Walk a's ancestry breadth-first (newest first).
	seen := make(map[oid]bool)
	queue = []oid{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if bAnc[cur] {
			return cur, true
		}
		if o := r.objs[cur]; o != nil && o.typ == objCommit {
			queue = append(queue, parseCommit(o.data).parents...)
		}
	}
	return oid{}, false
}

// diffFiles computes the file list between baseTree and headTree.
func (r *Repo) diffFiles(baseTree, headTree oid) []pullFile {
	base := make(map[string]treeEnt)
	head := make(map[string]treeEnt)
	if baseTree != (oid{}) {
		r.flattenTree(baseTree, "", base)
	}
	if headTree != (oid{}) {
		r.flattenTree(headTree, "", head)
	}
	names := make(map[string]bool)
	for n := range base {
		names[n] = true
	}
	for n := range head {
		names[n] = true
	}
	var out []pullFile
	for n := range names {
		b, inBase := base[n]
		h, inHead := head[n]
		switch {
		case inBase && !inHead:
			lines := 0
			if o := r.objs[b.id]; o != nil {
				lines = countLines(o.data)
			}
			out = append(out, pullFile{name: n, status: "removed", additions: 0, deletions: lines})
		case !inBase && inHead:
			lines := 0
			if o := r.objs[h.id]; o != nil {
				lines = countLines(o.data)
			}
			out = append(out, pullFile{name: n, status: "added", additions: lines, deletions: 0})
		case b.id != h.id:
			add, del := 0, 0
			if bo := r.objs[b.id]; bo != nil {
				del = countLines(bo.data)
			}
			if ho := r.objs[h.id]; ho != nil {
				add = countLines(ho.data)
			}
			out = append(out, pullFile{name: n, status: "modified", additions: add, deletions: del})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// threeWayMerge attempts to merge head into baseTip with mergeBase as the
// ancestor. Returns the merged tree id and whether it conflicted.
func (r *Repo) threeWayMerge(baseTip, headTip oid) (oid, bool) {
	base, ok := r.mergeBase(baseTip, headTip)
	if !ok {
		return oid{}, true
	}
	baseTree := oid{}
	headTree := oid{}
	tipTree := oid{}
	if o := r.objs[base]; o != nil && o.typ == objCommit {
		baseTree = parseCommit(o.data).tree
	}
	if o := r.objs[headTip]; o != nil && o.typ == objCommit {
		headTree = parseCommit(o.data).tree
	}
	if o := r.objs[baseTip]; o != nil && o.typ == objCommit {
		tipTree = parseCommit(o.data).tree
	}
	baseMap := make(map[string]treeEnt)
	headMap := make(map[string]treeEnt)
	tipMap := make(map[string]treeEnt)
	if baseTree != (oid{}) {
		r.flattenTree(baseTree, "", baseMap)
	}
	if headTree != (oid{}) {
		r.flattenTree(headTree, "", headMap)
	}
	if tipTree != (oid{}) {
		r.flattenTree(tipTree, "", tipMap)
	}
	merged := make(map[string]treeEnt)
	for p, e := range tipMap {
		merged[p] = e
	}
	conflict := false
	names := make(map[string]bool)
	for p := range baseMap {
		names[p] = true
	}
	for p := range headMap {
		names[p] = true
	}
	for p := range names {
		b, inBase := baseMap[p]
		h, inHead := headMap[p]
		t, inTip := tipMap[p]
		headChanged := !inBase && inHead || inBase && (!inHead || h.id != b.id)
		tipChanged := !inBase && inTip || inBase && (!inTip || t.id != b.id)
		switch {
		case !headChanged:
			// keep tip version (already in merged)
		case !tipChanged:
			if inHead {
				merged[p] = h
			} else {
				delete(merged, p)
			}
		default:
			// Both changed: take head's version only if identical to tip's.
			if inHead == inTip && (!inHead || h.id == t.id) {
				// same change, keep
			} else {
				conflict = true
			}
		}
	}
	if conflict {
		return oid{}, true
	}
	// Rebuild nested trees from the flat map.
	return r.buildTreeFromFlat(merged), false
}

// buildTreeFromFlat materializes nested tree objects from a flat path map.
func (r *Repo) buildTreeFromFlat(flat map[string]treeEnt) oid {
	// Group by directory.
	type dir struct {
		ents map[string]treeEnt
		subs map[string]*dir
	}
	root := &dir{ents: make(map[string]treeEnt), subs: make(map[string]*dir)}
	for path, e := range flat {
		parts := splitPath(path)
		d := root
		for _, p := range parts[:len(parts)-1] {
			sub, ok := d.subs[p]
			if !ok {
				sub = &dir{ents: make(map[string]treeEnt), subs: make(map[string]*dir)}
				d.subs[p] = sub
			}
			d = sub
		}
		d.ents[parts[len(parts)-1]] = e
	}
	var build func(d *dir) oid
	build = func(d *dir) oid {
		var ents []treeEnt
		for _, e := range d.ents {
			ents = append(ents, e)
		}
		for name, sub := range d.subs {
			ents = append(ents, treeEnt{mode: "40000", name: name, id: build(sub)})
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].name < ents[j].name })
		data := buildTree(ents)
		id := hashObject(objTree, data)
		r.objs[id] = &obj{typ: objTree, data: data}
		return id
	}
	return build(root)
}

// packKeyFor computes the cache key for a fetch response. The oid lists
// are sorted first so equivalent requests share one entry.
func packKeyFor(ver uint64, proto int, wants, haves, shallow []oid, depth int) packKey {
	h := sha1.New()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(ver >> (56 - 8*i))
	}
	h.Write(b[:])
	h.Write([]byte{byte(proto), byte(depth)})
	sorted := func(in []oid) []oid {
		out := make([]oid, len(in))
		copy(out, in)
		sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
		return out
	}
	for _, w := range sorted(wants) {
		h.Write(w[:])
	}
	h.Write([]byte{0})
	for _, w := range sorted(haves) {
		h.Write(w[:])
	}
	h.Write([]byte{0})
	for _, w := range sorted(shallow) {
		h.Write(w[:])
	}
	var k packKey
	k.ver = ver
	copy(k.key[:], h.Sum(nil))
	return k
}

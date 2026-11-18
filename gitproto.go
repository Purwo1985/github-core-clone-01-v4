// Git smart HTTP: protocol v2/v0 advertisements, ls-refs, fetch and
// receive-pack, per the competition wire spec.
package main

import (
	"bytes"
	"sort"
	"strings"
)

const agentName = "github-core-clone-01-v4/1.0.0"

// --- advertisements -------------------------------------------------------

// advV2 builds the upload-pack v2 advertisement for a repo.
func (r *Repo) advV2() []byte {
	if r.advV2Ver == r.version && r.advV2Cache != nil {
		return r.advV2Cache
	}
	var b bytes.Buffer
	b.Write(pktLineStr("# service=git-upload-pack\n"))
	b.Write(pktFlush)
	b.Write(pktLineStr("version 2\n"))
	b.Write(pktLineStr("fetch=shallow\n"))
	b.Write(pktLineStr("ls-refs=unborn\n"))
	b.Write(pktLineStr("agent=" + agentName + "\n"))
	b.Write(pktLineStr("object-format=sha1\n"))
	b.Write(pktFlush)
	r.advV2Cache = b.Bytes()
	r.advV2Ver = r.version
	return r.advV2Cache
}

// advV0 builds the upload-pack v0 advertisement for a repo.
func (r *Repo) advV0() []byte {
	if r.advV0Ver == r.version && r.advV0Cache != nil {
		return r.advV0Cache
	}
	var b bytes.Buffer
	b.Write(pktLineStr("# service=git-upload-pack\n"))
	b.Write(pktFlush)
	caps := "symref=HEAD:refs/heads/" + r.defaultBranch + " side-band-64k thin-pack ofs-delta shallow agent=" + agentName
	tip, ok := r.defaultTip()
	if !ok {
		b.Write(pktLineStr(zeroHex + " capabilities^{}\x00" + caps + "\n"))
	} else {
		b.Write(pktLineStr(tip.hex() + " HEAD\x00" + caps + "\n"))
		for _, name := range r.sortedRefNames() {
			id := r.refs[name]
			b.Write(pktLineStr(id.hex() + " " + name + "\n"))
			// Peeled line for annotated tags.
			if strings.HasPrefix(name, "refs/tags/") {
				if o := r.objs[id]; o != nil && o.typ == objTag {
					if target, _, ok := parseTag(o.data); ok {
						b.Write(pktLineStr(target.hex() + " " + name + "^{}\n"))
					}
				}
			}
		}
	}
	b.Write(pktFlush)
	r.advV0Cache = b.Bytes()
	r.advV0Ver = r.version
	return r.advV0Cache
}

// advReceive builds the receive-pack v0 advertisement.
func (r *Repo) advReceive() []byte {
	var b bytes.Buffer
	b.Write(pktLineStr("# service=git-receive-pack\n"))
	b.Write(pktFlush)
	caps := "report-status side-band-64k delete-refs ofs-delta agent=" + agentName
	tip, ok := r.defaultTip()
	if !ok {
		b.Write(pktLineStr(zeroHex + " capabilities^{}\x00" + caps + "\n"))
	} else {
		b.Write(pktLineStr(tip.hex() + " HEAD\x00" + caps + "\n"))
		for _, name := range r.sortedRefNames() {
			b.Write(pktLineStr(r.refs[name].hex() + " " + name + "\n"))
		}
	}
	b.Write(pktFlush)
	return b.Bytes()
}

// handleInfoRefs answers GET /{owner}/{repo}/info/refs?service=...
func handleInfoRefs(req *request) *response {
	owner, repoName := req.seg[0], req.seg[1]
	service, _ := queryParam(req.query, "service")
	storeMu()
	defer storeUnlock()
	if owner != viewer.login {
		return &response{status: 404, body: jsonError("Not Found")}
	}
	r := gstore.getRepo(repoName)
	if r == nil {
		return &response{status: 404, body: jsonError("Not Found")}
	}
	switch service {
	case "git-upload-pack":
		if req.gitProtocol == "version=2" {
			return &response{status: 200, ctype: "application/x-git-upload-pack-advertisement", body: r.advV2()}
		}
		return &response{status: 200, ctype: "application/x-git-upload-pack-advertisement", body: r.advV0()}
	case "git-receive-pack":
		return &response{status: 200, ctype: "application/x-git-receive-pack-advertisement", body: r.advReceive()}
	default:
		return &response{status: 400, body: jsonError("unknown service")}
	}
}

// handleGitService answers POST /{owner}/{repo}/git-upload-pack and
// git-receive-pack.
func handleGitService(req *request) *response {
	owner, repoName := req.seg[0], req.seg[1]
	storeMu()
	defer storeUnlock()
	if owner != viewer.login {
		return &response{status: 404, body: jsonError("Not Found")}
	}
	r := gstore.getRepo(repoName)
	if r == nil {
		return &response{status: 404, body: jsonError("Not Found")}
	}
	if req.seg[2] == "git-receive-pack" {
		return r.handleReceivePack(req)
	}
	return r.handleUploadPack(req)
}

// --- upload-pack ----------------------------------------------------------

// handleUploadPack serves ls-refs and fetch (v2, with a v0 fallback).
func (r *Repo) handleUploadPack(req *request) *response {
	packets, err := decodePktLines(req.body)
	if err != nil || len(packets) == 0 || packets[0].data == nil {
		return &response{status: 400, ctype: "text/plain", body: []byte("malformed request")}
	}
	cmd := strings.TrimSuffix(string(packets[0].data), "\n")
	switch cmd {
	case "command=ls-refs":
		return r.handleLsRefs(packets[1:])
	case "command=fetch":
		return r.handleFetchV2(packets[1:])
	default:
		// v0 fetch: want/have lines.
		if strings.HasPrefix(cmd, "want ") {
			return r.handleFetchV0(packets)
		}
		return &response{status: 400, ctype: "text/plain", body: []byte("unknown command")}
	}
}

// handleLsRefs answers a v2 ls-refs command.
func (r *Repo) handleLsRefs(packets []pkt) *response {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, p := range packets {
		if p.flush || p.delim {
			continue
		}
		line := strings.TrimSuffix(string(p.data), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	// The common case (peel + symrefs + unborn, no prefix) is cached.
	if peel && symrefs && unborn && len(prefixes) == 0 {
		if r.lsVer == r.version && r.lsDefault != nil {
			return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: r.lsDefault}
		}
	}
	var b bytes.Buffer
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	tip, hasTip := r.defaultTip()
	// HEAD first.
	if keep("HEAD") {
		if !hasTip {
			if unborn {
				b.Write(pktLineStr("unborn HEAD symref-target:refs/heads/" + r.defaultBranch + "\n"))
			}
		} else if symrefs {
			b.Write(pktLineStr(tip.hex() + " HEAD symref-target:refs/heads/" + r.defaultBranch + "\n"))
		} else {
			b.Write(pktLineStr(tip.hex() + " HEAD\n"))
		}
	}
	for _, name := range r.sortedRefNames() {
		if !keep(name) {
			continue
		}
		id := r.refs[name]
		line := id.hex() + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if o := r.objs[id]; o != nil && o.typ == objTag {
				if target, _, ok := parseTag(o.data); ok {
					line += " peeled:" + target.hex()
				}
			}
		}
		b.Write(pktLineStr(line + "\n"))
	}
	b.Write(pktFlush)
	out := b.Bytes()
	if peel && symrefs && unborn && len(prefixes) == 0 {
		r.lsDefault = out
		r.lsVer = r.version
	}
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

// fetchReq is a parsed fetch request (v2 or v0).
type fetchReq struct {
	wants          []oid
	haves          []oid
	shallow        []oid // client-declared shallow boundaries
	depth          int
	hasDepth       bool
	deepenRelative bool
	done           bool
}

func parseFetchPackets(packets []pkt, v2 bool) fetchReq {
	var f fetchReq
	for _, p := range packets {
		if p.flush || p.delim || p.data == nil {
			continue
		}
		line := strings.TrimSuffix(string(p.data), "\n")
		// v0 wants carry capabilities after a NUL on the first line.
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		switch {
		case strings.HasPrefix(line, "want "):
			if id, ok := parseOIDHex(line[5:]); ok {
				f.wants = append(f.wants, id)
			}
		case strings.HasPrefix(line, "have "):
			if id, ok := parseOIDHex(line[5:]); ok {
				f.haves = append(f.haves, id)
			}
		case strings.HasPrefix(line, "shallow "):
			if id, ok := parseOIDHex(line[8:]); ok {
				f.shallow = append(f.shallow, id)
			}
		case strings.HasPrefix(line, "deepen "):
			var n int
			for _, c := range line[7:] {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			f.depth = n
			f.hasDepth = true
		case line == "deepen-relative":
			f.deepenRelative = true
		case line == "done":
			f.done = true
		}
	}
	_ = v2
	return f
}

// fetchResult is the computed object set and shallow boundary info.
type fetchResult struct {
	toSend     map[oid]*obj // objects to include in the pack
	newShallow []oid
	unshallow  []oid
	allHeld    bool // every want is already held by the client
}

// computeFetch resolves wants/haves/depth into the object set to send.
func (r *Repo) computeFetch(f fetchReq) fetchResult {
	var res fetchResult
	// Effective depth: absolute when the client sends `deepen N`, relative
	// to the client's current boundaries when it sends `deepen-relative`.
	depth := f.depth
	if f.hasDepth && f.deepenRelative {
		depth = f.depth + 1 // at least one generation deeper than now
		for _, s := range f.shallow {
			for _, w := range f.wants {
				if d, ok := r.commitDistance(w, s); ok && d+f.depth+1 > depth {
					depth = d + f.depth + 1
				}
			}
		}
	}
	// wantClosure: full closure when no depth, depth-limited otherwise.
	var wantClosure map[oid]*obj
	var wantDepths map[oid]int
	if f.hasDepth {
		wantClosure, wantDepths = r.closure(f.wants, depth)
	} else {
		wantClosure, wantDepths = r.closure(f.wants, 0)
	}
	// haveClosure: everything the client provably holds. The walk starts
	// at the haves but never follows the parents of a commit in the
	// client's shallow set — the client holds that boundary commit itself,
	// not its ancestors. (A plain full closure would wrongly assume the
	// client holds history below its shallow boundary.)
	haveClosure := make(map[oid]*obj)
	if len(f.haves) > 0 {
		shallowSet := make(map[oid]bool, len(f.shallow))
		for _, s := range f.shallow {
			shallowSet[s] = true
		}
		haveClosure = r.closureCutoff(f.haves, shallowSet)
	}
	res.toSend = make(map[oid]*obj, len(wantClosure))
	for id, o := range wantClosure {
		if _, held := haveClosure[id]; !held {
			res.toSend[id] = o
		}
	}
	// allHeld: every wanted object is already in the client's closure.
	if len(f.haves) > 0 {
		res.allHeld = true
		for id := range wantClosure {
			if _, ok := haveClosure[id]; !ok {
				res.allHeld = false
				break
			}
		}
	}
	// Shallow-info: boundaries are the deepest included commits (depth
	// N-1 for a deepen-N request), i.e. the wants themselves at depth 1.
	if f.hasDepth {
		for id, d := range wantDepths {
			if d == depth-1 {
				res.newShallow = append(res.newShallow, id)
			}
		}
		sort.Slice(res.newShallow, func(i, j int) bool {
			return bytes.Compare(res.newShallow[i][:], res.newShallow[j][:]) < 0
		})
	}
	// unshallow: old boundaries that are now interior (their depth in the
	// new closure is strictly less than the new boundary depth).
	for _, s := range f.shallow {
		if d, ok := wantDepths[s]; ok && d < depth-1 {
			res.unshallow = append(res.unshallow, s)
		}
	}
	sort.Slice(res.unshallow, func(i, j int) bool {
		return bytes.Compare(res.unshallow[i][:], res.unshallow[j][:]) < 0
	})
	return res
}

// sidebandPack wraps pack bytes into sideband band-1 pkt-lines.
func sidebandPack(pack []byte) []byte {
	var b bytes.Buffer
	for len(pack) > 0 {
		chunk := pack
		if len(chunk) > 65515 {
			chunk = chunk[:65515]
		}
		payload := make([]byte, 0, len(chunk)+1)
		payload = append(payload, 1)
		payload = append(payload, chunk...)
		b.Write(pktLine(payload))
		pack = pack[len(chunk):]
	}
	return b.Bytes()
}

// handleFetchV2 answers a v2 fetch command.
func (r *Repo) handleFetchV2(packets []pkt) *response {
	f := parseFetchPackets(packets, true)
	if len(f.wants) == 0 {
		return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: pktFlush}
	}
	// Cache the whole response for identical requests on the same version.
	key := packKeyFor(r.version, 2, f.wants, f.haves, f.shallow, boolToDepth(f))
	if cached, ok := r.packs[key]; ok {
		return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: cached}
	}
	res := r.computeFetch(f)
	// Negotiation round: the client sent haves but no `done`, and not
	// every want is held — answer NAK and stop; the client will send
	// another round (more haves, or `done`).
	if len(f.haves) > 0 && !f.done && !res.allHeld {
		var b bytes.Buffer
		b.Write(pktLineStr("acknowledgments\n"))
		b.Write(pktLineStr("NAK\n"))
		b.Write(pktFlush)
		return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: b.Bytes()}
	}
	out := r.renderFetchV2(f, res)
	r.packs[key] = out
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

func boolToDepth(f fetchReq) int {
	if f.hasDepth {
		return f.depth
	}
	return 0
}

// renderFetchV2 builds the sectioned v2 fetch response. Per the wire
// protocol, the acknowledgments section only appears when the client sent
// haves; without haves the response starts at shallow-info (or packfile).
func (r *Repo) renderFetchV2(f fetchReq, res fetchResult) []byte {
	var b bytes.Buffer
	if len(f.haves) > 0 {
		b.Write(pktLineStr("acknowledgments\n"))
		if res.allHeld {
			for _, w := range f.wants {
				b.Write(pktLineStr("ACK " + w.hex() + "\n"))
			}
			b.Write(pktLineStr("ready\n"))
		} else {
			b.Write(pktLineStr("NAK\n"))
		}
		b.Write(pktDelim)
	}
	// shallow-info
	if f.hasDepth || len(f.shallow) > 0 {
		b.Write(pktLineStr("shallow-info\n"))
		for _, s := range res.newShallow {
			b.Write(pktLineStr("shallow " + s.hex() + "\n"))
		}
		for _, s := range res.unshallow {
			b.Write(pktLineStr("unshallow " + s.hex() + "\n"))
		}
		b.Write(pktDelim)
	}
	// packfile
	b.Write(pktLineStr("packfile\n"))
	var objs []packObj
	for id, o := range res.toSend {
		objs = append(objs, packObj{id: id, typ: o.typ, data: o.data})
	}
	pack := writePack(objs)
	b.Write(sidebandPack(pack))
	b.Write(pktFlush)
	return b.Bytes()
}

// handleFetchV0 answers a v0 fetch (same semantics, flat framing).
func (r *Repo) handleFetchV0(packets []pkt) *response {
	f := parseFetchPackets(packets, false)
	if len(f.wants) == 0 {
		return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: pktFlush}
	}
	key := packKeyFor(r.version, 0, f.wants, f.haves, f.shallow, boolToDepth(f))
	if cached, ok := r.packs[key]; ok {
		return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: cached}
	}
	res := r.computeFetch(f)
	var b bytes.Buffer
	if len(f.haves) > 0 && res.allHeld {
		for _, w := range f.wants {
			b.Write(pktLineStr("ACK " + w.hex() + "\n"))
		}
	} else {
		b.Write(pktLineStr("NAK\n"))
	}
	for _, s := range res.newShallow {
		b.Write(pktLineStr("shallow " + s.hex() + "\n"))
	}
	for _, s := range res.unshallow {
		b.Write(pktLineStr("unshallow " + s.hex() + "\n"))
	}
	var objs []packObj
	for id, o := range res.toSend {
		objs = append(objs, packObj{id: id, typ: o.typ, data: o.data})
	}
	pack := writePack(objs)
	b.Write(sidebandPack(pack))
	b.Write(pktFlush)
	out := b.Bytes()
	r.packs[key] = out
	return &response{status: 200, ctype: "application/x-git-upload-pack-result", body: out}
}

// --- receive-pack ---------------------------------------------------------

type pushCmd struct {
	oldID oid
	newID oid
	ref   string
}

// handleReceivePack applies a push: parse commands, read the pack, apply
// fast-forward rules, report per-ref results over sideband.
func (r *Repo) handleReceivePack(req *request) *response {
	// Parse the command section only (up to the first flush); the bytes
	// after it are the raw pack, not pkt-lines.
	var cmds []pushCmd
	packStart := len(req.body)
	pos := 0
	for pos+4 <= len(req.body) {
		if bytes.Equal(req.body[pos:pos+4], []byte("0000")) {
			packStart = pos + 4
			break
		}
		length := 0
		for k := 0; k < 4; k++ {
			v, ok := hexVal(req.body[pos+k])
			if !ok {
				return &response{status: 400, ctype: "text/plain", body: []byte("malformed request")}
			}
			length = length*16 + int(v)
		}
		if length < 4 || pos+length > len(req.body) {
			return &response{status: 400, ctype: "text/plain", body: []byte("malformed request")}
		}
		line := string(req.body[pos+4 : pos+length])
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			oldID, ok1 := parseOIDHex(parts[0])
			newID, ok2 := parseOIDHex(parts[1])
			if ok1 && ok2 {
				cmds = append(cmds, pushCmd{oldID: oldID, newID: newID, ref: parts[2]})
			}
		}
		pos += length
	}
	if len(cmds) == 0 {
		return &response{status: 400, ctype: "text/plain", body: []byte("no commands")}
	}
	// Unpack.
	unpackOk := true
	unpackDetail := "ok"
	packBytes := req.body[packStart:]
	needPack := false
	for _, c := range cmds {
		if c.newID != (oid{}) {
			needPack = true
			break
		}
	}
	if needPack {
		if len(packBytes) == 0 {
			unpackOk = false
			unpackDetail = "empty pack"
		} else if !verifyPackChecksum(packBytes) {
			unpackOk = false
			unpackDetail = "pack checksum mismatch"
		} else {
			objs, err := readPack(packBytes, func(id oid) *obj { return r.objs[id] })
			if err != nil {
				unpackOk = false
				unpackDetail = err.Error()
			} else {
				for id, o := range objs {
					r.objs[id] = o
				}
			}
		}
	}
	// Apply refs.
	var report bytes.Buffer
	report.WriteString("unpack " + unpackDetail + "\n")
	for _, c := range cmds {
		ok := false
		reason := ""
		if !unpackOk {
			reason = "unpack failed"
		} else {
			ok, reason = r.applyPush(c)
		}
		if ok {
			report.WriteString("ok " + c.ref + "\n")
		} else {
			report.WriteString("ng " + c.ref + " " + reason + "\n")
		}
	}
	// Report over sideband band 1: the band-1 stream is itself a pkt-line
	// stream — one inner pkt-line per report line, then an inner flush —
	// carried in a single outer sideband packet.
	var inner bytes.Buffer
	for _, line := range strings.SplitAfter(report.String(), "\n") {
		if line == "" {
			continue
		}
		inner.Write(pktLineStr(line))
	}
	inner.Write(pktFlush)
	var outer bytes.Buffer
	outer.Write(pktLine(append([]byte{1}, inner.Bytes()...)))
	outer.Write(pktFlush)
	if len(cmds) > 0 {
		r.bump()
	}
	return &response{status: 200, ctype: "application/x-git-receive-pack-result", body: outer.Bytes()}
}

// applyPush enforces the fast-forward rule for one command.
func (r *Repo) applyPush(c pushCmd) (bool, string) {
	cur, exists := r.refs[c.ref]
	switch {
	case c.newID == (oid{}): // delete
		if !exists {
			return false, "non-fast-forward"
		}
		if cur != c.oldID {
			return false, "non-fast-forward"
		}
		delete(r.refs, c.ref)
		return true, ""
	case !exists: // create
		if c.oldID != (oid{}) {
			return false, "non-fast-forward"
		}
		if r.objs[c.newID] == nil {
			return false, "non-fast-forward"
		}
		r.refs[c.ref] = c.newID
		return true, ""
	default: // update
		if cur != c.oldID {
			return false, "non-fast-forward"
		}
		if r.objs[c.newID] == nil {
			return false, "non-fast-forward"
		}
		if !r.isAncestor(cur, c.newID) {
			return false, "non-fast-forward"
		}
		r.refs[c.ref] = c.newID
		return true, ""
	}
}

// --- store locking helpers (defined here to keep handlers short) ----------

func storeMu()    { gstore.mu.Lock() }
func storeUnlock() { gstore.mu.Unlock() }

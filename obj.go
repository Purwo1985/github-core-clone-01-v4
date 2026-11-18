// Git objects: SHA-1 ids, tree/commit/tag encoding, pkt-lines, packfiles
// (read with delta resolution, write with fast deflate) and reachability.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"errors"
	"io"
	"sort"
)

const (
	objCommit = byte('c')
	objTree   = byte('t')
	objBlob   = byte('b')
	objTag    = byte('g')
)

var zeroOID [20]byte

const zeroHex = "0000000000000000000000000000000000000000"

// oid is a SHA-1 object id.
type oid [20]byte

func (o oid) hex() string {
	var b [40]byte
	for i, v := range o {
		b[i*2] = hexDigits[v>>4]
		b[i*2+1] = hexDigits[v&0xf]
	}
	return string(b[:])
}

func parseOIDHex(s string) (oid, bool) {
	if len(s) != 40 {
		return oid{}, false
	}
	var o oid
	for i := 0; i < 20; i++ {
		hi, ok1 := hexVal(s[i*2])
		lo, ok2 := hexVal(s[i*2+1])
		if !ok1 || !ok2 {
			return oid{}, false
		}
		o[i] = hi<<4 | lo
	}
	return o, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

type obj struct {
	typ  byte
	data []byte
}

// hashObject returns the git id of a (type, data) pair.
func hashObject(typ byte, data []byte) oid {
	h := sha1.New()
	var hdr [32]byte
	n := 0
	switch typ {
	case objCommit:
		n += copy(hdr[n:], "commit")
	case objTree:
		n += copy(hdr[n:], "tree")
	case objBlob:
		n += copy(hdr[n:], "blob")
	case objTag:
		n += copy(hdr[n:], "tag")
	}
	hdr[n] = ' '
	n++
	n += appendIntInto(hdr[n:], len(data))
	hdr[n] = 0
	n++
	h.Write(hdr[:n])
	h.Write(data)
	var out oid
	copy(out[:], h.Sum(nil))
	return out
}

func appendIntInto(b []byte, n int) int {
	if n == 0 {
		b[0] = '0'
		return 1
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return copy(b, tmp[i:])
}

// --- pkt-lines ------------------------------------------------------------

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	var out [4]byte
	out[0] = hexDigits[(n>>12)&0xf]
	out[1] = hexDigits[(n>>8)&0xf]
	out[2] = hexDigits[(n>>4)&0xf]
	out[3] = hexDigits[n&0xf]
	b := make([]byte, 0, n)
	b = append(b, out[:]...)
	return append(b, payload...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLines splits body into pkt-line payloads. Returns payloads in order;
// flush/delim markers are returned as nil payloads with kind set.
type pkt struct {
	flush bool
	delim bool
	data  []byte // nil for flush/delim
}

// decodePktLines parses a pkt-line stream. A malformed length aborts.
func decodePktLines(b []byte) ([]pkt, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("short pkt-line")
		}
		if b[0] == '0' && b[1] == '0' && b[2] == '0' && b[3] == '0' {
			out = append(out, pkt{flush: true})
			b = b[4:]
			continue
		}
		if b[0] == '0' && b[1] == '0' && b[2] == '0' && b[3] == '1' {
			out = append(out, pkt{delim: true})
			b = b[4:]
			continue
		}
		n := 0
		for i := 0; i < 4; i++ {
			v, ok := hexVal(b[i])
			if !ok {
				return nil, errors.New("bad pkt-line length")
			}
			n = n*16 + int(v)
		}
		if n < 4 || n > len(b) {
			return nil, errors.New("pkt-line length out of range")
		}
		out = append(out, pkt{data: b[4:n]})
		b = b[n:]
	}
	return out, nil
}

// --- trees and commits ----------------------------------------------------

type treeEnt struct {
	mode string
	name string
	id   oid
}

// parseTree decodes a tree object body into entries (in stored order).
func parseTree(data []byte) []treeEnt {
	var ents []treeEnt
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		var id oid
		copy(id[:], data[i:i+20])
		i += 20
		ents = append(ents, treeEnt{mode: mode, name: name, id: id})
	}
	return ents
}

func buildTree(ents []treeEnt) []byte {
	var b bytes.Buffer
	for _, e := range ents {
		b.WriteString(e.mode)
		b.WriteByte(' ')
		b.WriteString(e.name)
		b.WriteByte(0)
		b.Write(e.id[:])
	}
	return b.Bytes()
}

type commitInfo struct {
	tree      oid
	parents   []oid
	message   string
	author    string
	committer string
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	// Header ends at the first blank line.
	hdrEnd := bytes.Index(data, []byte("\n\n"))
	hdr := data
	if hdrEnd >= 0 {
		hdr = data[:hdrEnd]
		c.message = string(data[hdrEnd+2:])
	}
	for _, line := range bytes.Split(hdr, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			if id, ok := parseOIDHex(string(line[5:])); ok {
				c.tree = id
			}
		case bytes.HasPrefix(line, []byte("parent ")):
			if id, ok := parseOIDHex(string(line[7:])); ok {
				c.parents = append(c.parents, id)
			}
		case bytes.HasPrefix(line, []byte("author ")):
			c.author = string(line[7:])
		case bytes.HasPrefix(line, []byte("committer ")):
			c.committer = string(line[10:])
		}
	}
	return c
}

func buildCommit(tree oid, parents []oid, author, committer, message string) []byte {
	var b bytes.Buffer
	b.WriteString("tree ")
	b.WriteString(tree.hex())
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p.hex())
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(author)
	b.WriteByte('\n')
	b.WriteString("committer ")
	b.WriteString(committer)
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString(message)
	return b.Bytes()
}

// parseTag extracts the object a tag points at and its type.
func parseTag(data []byte) (oid, string, bool) {
	var target oid
	typ := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			id, ok := parseOIDHex(string(line[7:]))
			if !ok {
				return oid{}, "", false
			}
			target = id
		} else if bytes.HasPrefix(line, []byte("type ")) {
			typ = string(line[5:])
		} else if len(line) == 0 {
			break
		}
	}
	return target, typ, true
}

// --- packfiles ------------------------------------------------------------

// readPack parses a PACK v2 buffer. Delta entries are resolved against the
// pack itself and, when needed, lookup (objects already in the repo).
// The returned map is keyed by object id.
func readPack(pack []byte, lookup func(oid) *obj) (map[oid]*obj, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	if pack[4] != 0 || pack[5] != 0 || pack[6] != 0 || pack[7] != 2 {
		return nil, errors.New("unsupported pack version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	if len(pack) < 12+20 {
		return nil, errors.New("short packfile")
	}
	type entry struct {
		typ    byte
		data   []byte // inflated payload (delta instructions for delta types)
		ofs    int64  // base offset for OFS_DELTA, -1 otherwise
		ref    oid    // base id for REF_DELTA
		offset int    // pack offset where this entry starts
		isBase bool
	}
	entries := make([]*entry, 0, count)
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= len(pack) {
			return nil, errors.New("pack truncated")
		}
		start := pos
		c := pack[pos]
		pos++
		typ := (c >> 4) & 7
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= len(pack) {
				return nil, errors.New("pack truncated")
			}
			c = pack[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		var e entry
		e.offset = start
		switch typ {
		case 1:
			e.typ = objCommit
		case 2:
			e.typ = objTree
		case 3:
			e.typ = objBlob
		case 4:
			e.typ = objTag
		case 6: // OFS_DELTA
			c = pack[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				c = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			e.ofs = int64(start) - off
			e.typ = 0
		case 7: // REF_DELTA
			if pos+20 > len(pack) {
				return nil, errors.New("pack truncated")
			}
			copy(e.ref[:], pack[pos:pos+20])
			pos += 20
			e.typ = 0
		default:
			return nil, errors.New("unknown pack entry type")
		}
		// Inflate the zlib stream starting at pos, counting the bytes the
		// deflate stream consumes so the next entry starts at the right place.
		data, consumed, err := inflateZlib(pack[pos:])
		if err != nil {
			return nil, err
		}
		pos += consumed
		if int64(len(data)) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		e.data = data
		entries = append(entries, &e)
	}
	// Resolve deltas.
	byOffset := make(map[int64]*entry, len(entries))
	for _, e := range entries {
		byOffset[int64(e.offset)] = e
	}
	out := make(map[oid]*obj, count)
	var resolve func(e *entry) (*obj, error)
	resolve = func(e *entry) (*obj, error) {
		if e.isBase {
			return &obj{typ: e.typ, data: e.data}, nil
		}
		if e.typ != 0 {
			e.isBase = true
			return &obj{typ: e.typ, data: e.data}, nil
		}
		var base *obj
		if e.ofs != 0 {
			be, ok := byOffset[e.ofs]
			if !ok {
				return nil, errors.New("ofs-delta base not in pack")
			}
			var err error
			base, err = resolve(be)
			if err != nil {
				return nil, err
			}
		} else {
			// REF_DELTA: base may be in this pack (plain or itself a delta)
			// or already in the repo.
			for _, cand := range entries {
				if cand == e {
					continue
				}
				co, err := resolve(cand)
				if err != nil {
					return nil, err
				}
				if hashObject(co.typ, co.data) == e.ref {
					base = co
					break
				}
			}
			if base == nil && lookup != nil {
				base = lookup(e.ref)
			}
			if base == nil {
				return nil, errors.New("ref-delta base missing")
			}
		}
		applied, err := applyDelta(base.data, e.data)
		if err != nil {
			return nil, err
		}
		e.typ = base.typ
		e.data = applied
		e.isBase = true
		return &obj{typ: base.typ, data: applied}, nil
	}
	for _, e := range entries {
		o, err := resolve(e)
		if err != nil {
			return nil, err
		}
		id := hashObject(o.typ, o.data)
		out[id] = o
	}
	return out, nil
}

// inflateZlib inflates one zlib stream at the start of b and returns the
// payload plus the total stream length. It relies on zlib reading through
// a bytes.Reader (an io.ByteReader), which makes the decompressor consume
// exactly the stream bytes — no over-read into the next entry.
func inflateZlib(b []byte) ([]byte, int, error) {
	if len(b) < 2 {
		return nil, 0, errors.New("short zlib stream")
	}
	br := bytes.NewReader(b)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(b) - br.Len()
	return data, consumed, nil
}

// applyDelta applies git delta instructions to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	p := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if p >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			c := delta[p]
			p++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for p < len(delta) {
		op := delta[p]
		p++
		if op&0x80 != 0 {
			// copy from base
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if p >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					off |= int(delta[p]) << (8 * uint(i))
					p++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if p >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					size |= int(delta[p]) << (8 * uint(i))
					p++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if p+int(op) > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out = append(out, delta[p:p+int(op)]...)
			p += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

// packObj is one object to write into a packfile.
type packObj struct {
	id   oid
	typ  byte
	data []byte
}

// writePack builds a PACK v2 with plain (non-delta) objects, deflated at
// BestSpeed. Objects are emitted in id order for determinism.
func writePack(objs []packObj) []byte {
	sort.Slice(objs, func(i, j int) bool { return bytes.Compare(objs[i].id[:], objs[j].id[:]) < 0 })
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write([]byte{0, 0, 0, 2})
	var cnt [4]byte
	n := len(objs)
	cnt[0] = byte(n >> 24)
	cnt[1] = byte(n >> 16)
	cnt[2] = byte(n >> 8)
	cnt[3] = byte(n)
	b.Write(cnt[:])
	var zw bytes.Buffer
	for _, o := range objs {
		var hdr [32]byte
		hn := 0
		typeNum := 0
		switch o.typ {
		case objCommit:
			typeNum = 1
		case objTree:
			typeNum = 2
		case objBlob:
			typeNum = 3
		case objTag:
			typeNum = 4
		}
		size := len(o.data)
		first := byte((typeNum << 4) | (size & 0x0f))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		hdr[hn] = first
		hn++
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			hdr[hn] = b
			hn++
		}
		b.Write(hdr[:hn])
		zw.Reset()
		w, _ := zlib.NewWriterLevel(&zw, flate.BestSpeed)
		w.Write(o.data)
		w.Close()
		b.Write(zw.Bytes())
	}
	h := sha1.New()
	h.Write(b.Bytes())
	b.Write(h.Sum(nil))
	return b.Bytes()
}

// verifyPackChecksum reports whether the trailing SHA-1 matches the bytes.
func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	h := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(h[:], pack[len(pack)-20:])
}

// --- reachability ---------------------------------------------------------

// closure walks from tips through commits/trees/tags, collecting every
// reachable object. maxDepth limits commit generations (0 = unlimited).
// depthOf maps each visited commit to its generation distance from a tip.
func (r *Repo) closure(tips []oid, maxDepth int) (map[oid]*obj, map[oid]int) {
	seen := make(map[oid]*obj)
	depthOf := make(map[oid]int)
	type item struct {
		id    oid
		depth int
	}
	stack := make([]item, 0, 64)
	for _, t := range tips {
		if _, ok := seen[t]; !ok {
			stack = append(stack, item{t, 0})
		}
	}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		o := r.objs[it.id]
		if o == nil {
			continue
		}
		if _, ok := seen[it.id]; ok {
			continue
		}
		// Depth limits apply to commits only: a commit at or beyond the
		// limit is not sent at all (nor expanded). Trees and blobs of an
		// included commit are always sent in full.
		if o.typ == objCommit && maxDepth > 0 && it.depth >= maxDepth {
			continue
		}
		seen[it.id] = o
		switch o.typ {
		case objCommit:
			depthOf[it.id] = it.depth
			c := parseCommit(o.data)
			if _, ok := seen[c.tree]; !ok {
				stack = append(stack, item{c.tree, it.depth})
			}
			for _, p := range c.parents {
				if _, ok := seen[p]; !ok {
					stack = append(stack, item{p, it.depth + 1})
				}
			}
		case objTree:
			for _, e := range parseTree(o.data) {
				if _, ok := seen[e.id]; !ok {
					stack = append(stack, item{e.id, it.depth})
				}
			}
		case objTag:
			target, _, ok := parseTag(o.data)
			if ok {
				if _, ok := seen[target]; !ok {
					stack = append(stack, item{target, it.depth})
				}
			}
		}
	}
	return seen, depthOf
}

// commitDistance returns the number of parent steps from `from` to `to`
// (dereferencing tags), or false when `to` is not an ancestor of `from`.
func (r *Repo) commitDistance(from, to oid) (int, bool) {
	// Dereference tags.
	for {
		o := r.objs[from]
		if o == nil {
			return 0, false
		}
		if o.typ != objTag {
			break
		}
		target, _, ok := parseTag(o.data)
		if !ok {
			return 0, false
		}
		from = target
	}
	if from == to {
		return 0, true
	}
	type item struct {
		id    oid
		depth int
	}
	seen := map[oid]bool{from: true}
	queue := []item{{from, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		o := r.objs[cur.id]
		if o == nil || o.typ != objCommit {
			continue
		}
		for _, p := range parseCommit(o.data).parents {
			if p == to {
				return cur.depth + 1, true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, item{p, cur.depth + 1})
			}
		}
	}
	return 0, false
}

// closureCutoff walks from tips collecting every reachable object, but
// never follows the parents of a commit in cutoff (a client's shallow
// boundary: the client holds the boundary commit itself, not its ancestors).
func (r *Repo) closureCutoff(tips []oid, cutoff map[oid]bool) map[oid]*obj {
	seen := make(map[oid]*obj)
	stack := append([]oid{}, tips...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		o := r.objs[id]
		if o == nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = o
		switch o.typ {
		case objCommit:
			c := parseCommit(o.data)
			if _, ok := seen[c.tree]; !ok {
				stack = append(stack, c.tree)
			}
			if !cutoff[id] {
				for _, p := range c.parents {
					if _, ok := seen[p]; !ok {
						stack = append(stack, p)
					}
				}
			}
		case objTree:
			for _, e := range parseTree(o.data) {
				if _, ok := seen[e.id]; !ok {
					stack = append(stack, e.id)
				}
			}
		case objTag:
			if target, _, ok := parseTag(o.data); ok {
				if _, ok := seen[target]; !ok {
					stack = append(stack, target)
				}
			}
		}
	}
	return seen
}

// isAncestor reports whether anc is reachable from tip (following parents).
// Uses the stored commit depth for an O(1) reject when possible.
func (r *Repo) isAncestor(anc, tip oid) bool {
	if anc == tip {
		return true
	}
	// Walk parents of tip until we hit anc or run out. Linear chains (the
	// common push case) exit on the first step.
	seen := 0
	cur := tip
	for {
		o := r.objs[cur]
		if o == nil || o.typ != objCommit {
			return false
		}
		c := parseCommit(o.data)
		for _, p := range c.parents {
			if p == anc {
				return true
			}
		}
		if len(c.parents) == 0 {
			return false
		}
		cur = c.parents[0]
		seen++
		if seen > 100000 {
			return false
		}
	}
}

// commitDepth returns the generation number of a commit (0 for a root).
func (r *Repo) commitDepth(id oid) int {
	if d, ok := r.commitDepths[id]; ok {
		return d
	}
	o := r.objs[id]
	if o == nil || o.typ != objCommit {
		return 0
	}
	c := parseCommit(o.data)
	d := 0
	for _, p := range c.parents {
		if pd := r.commitDepth(p) + 1; pd > d {
			d = pd
		}
	}
	r.commitDepths[id] = d
	return d
}

// flattenTree returns path -> (mode, oid) for every blob under tree.
func (r *Repo) flattenTree(tree oid, prefix string, out map[string]treeEnt) {
	o := r.objs[tree]
	if o == nil || o.typ != objTree {
		return
	}
	for _, e := range parseTree(o.data) {
		path := prefix + e.name
		if e.mode == "40000" {
			r.flattenTree(e.id, path+"/", out)
		} else {
			out[path] = e
		}
	}
}

// Git object layer: SHA-1 ids, tree/commit/tag parsing and building,
// packfile reading (including delta resolution) and writing, reachability
// walks, ancestry tests and merge-base search. No I/O here; the store
// supplies object lookups.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

const ZeroOID = "0000000000000000000000000000000000000000"

// Obj is one git object: its type and its exact bytes.
type Obj struct {
	Type string
	Data []byte
}

// ObjLookup resolves an object id to its object, if present.
type ObjLookup func(oid string) (*Obj, bool)

// HashObject returns the git object id of type+data: SHA-1 over
// "<type> <len>\0" followed by the bytes. The header is written
// without fmt so a hot path (pack parsing, fetch planning) does not pay
// for formatting on every hash.
func HashObject(typ string, data []byte) string {
	h := sha1.New()
	io.WriteString(h, typ)
	io.WriteString(h, " ")
	var nb [20]byte
	n := len(data)
	i := len(nb)
	for n > 0 {
		i--
		nb[i] = byte('0' + n%10)
		n /= 10
	}
	if i == len(nb) {
		i--
		nb[i] = '0'
	}
	h.Write(nb[i:])
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func oidBytes(s string) ([]byte, error) {
	if !isOID(s) {
		return nil, fmt.Errorf("bad object id %q", s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ---------------------------------------------------------------- trees

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree splits a tree object's bytes into entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		mode := string(data[off:sp])
		for _, c := range mode {
			if c < '0' || c > '7' {
				return nil, fmt.Errorf("tree entry mode %q is not octal", mode)
			}
		}
		out = append(out, TreeEntry{
			Mode: mode,
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return out, nil
}

// treeLess orders entries the way git does: by name bytes, a directory
// compared as if its name ended in '/'.
func treeLess(a, b TreeEntry) bool {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return an < bn
}

// BuildTree encodes entries into a tree object's bytes, sorted git-style.
func BuildTree(entries []TreeEntry) ([]byte, error) {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return treeLess(sorted[i], sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		b, err := oidBytes(e.OID)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		buf.Write(b)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------- commits and tags

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit parses a commit object's bytes; refuses a missing tree.
func ParseCommit(data []byte) (*CommitView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	cv := &CommitView{}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		key, val := string(line), ""
		if sp >= 0 {
			key, val = string(line[:sp]), string(line[sp+1:])
		}
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		case "committer":
			cv.Committer = val
		}
	}
	if !isOID(cv.Tree) {
		return nil, errors.New("commit has no valid tree header")
	}
	for _, p := range cv.Parents {
		if !isOID(p) {
			return nil, errors.New("commit has a bad parent id")
		}
	}
	if blank >= 0 {
		cv.Message = string(data[blank+2:])
	}
	return cv, nil
}

// BuildCommit encodes a commit object. authorLine/committerLine are the
// full identity lines ("Name <email> <unix> +0000").
func BuildCommit(tree string, parents []string, authorLine, committerLine, message string) ([]byte, error) {
	if !isOID(tree) {
		return nil, errors.New("buildCommit needs a valid tree id")
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		if !isOID(p) {
			return nil, errors.New("buildCommit needs valid parent ids")
		}
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s\ncommitter %s\n\n", authorLine, committerLine)
	buf.WriteString(message)
	return buf.Bytes(), nil
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses an annotated tag object.
func ParseTag(data []byte) (*TagView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	tv := &TagView{}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tag header %q has no value", line)
		}
		switch string(line[:sp]) {
		case "object":
			tv.Object = string(line[sp+1:])
		case "type":
			tv.Type = string(line[sp+1:])
		case "tag":
			tv.Tag = string(line[sp+1:])
		case "tagger":
			tv.Tagger = string(line[sp+1:])
		}
	}
	if !isOID(tv.Object) {
		return nil, errors.New("tag has no valid object header")
	}
	switch tv.Type {
	case "commit", "tree", "blob", "tag":
	default:
		return nil, fmt.Errorf("tag has bad type %q", tv.Type)
	}
	if blank >= 0 {
		tv.Message = string(data[blank+2:])
	}
	return tv, nil
}

// ---------------------------------------------------------------- packfiles

var packTypeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// writePackObjectHeader encodes one pack entry header (type + size varint).
func writePackObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	first := (code << 4) | (size & 0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, byte(first|0x80))
		first = size & 0x7f
		size >>= 7
	}
	out = append(out, byte(first))
	return out
}

// WritePack encodes objects as a packfile version 2 with no deltas and a
// SHA-1 trailer over everything before it.
func WritePack(objs []*Obj) ([]byte, error) {
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4], hdr[5], hdr[6], hdr[7] = 0, 0, 0, 2
	n := len(objs)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	buf.Write(hdr)
	for _, o := range objs {
		buf.Write(writePackObjectHeader(o.Type, len(o.Data)))
		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(o.Data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...), nil
}

// PackEntry is one decoded pack entry with its resolved object id.
type PackEntry struct {
	OID  string
	Type string
	Data []byte
}

// readPackEntryHeader parses one entry's type/size varint at offset.
func readPackEntryHeader(b []byte, off int) (typ int, size int, next int, err error) {
	if off >= len(b) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	c := b[off]
	typ = int(c>>4) & 7
	size = int(c & 0x0f)
	shift := uint(4)
	off++
	for c&0x80 != 0 {
		if off >= len(b) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		c = b[off]
		off++
		size += int(c&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, errors.New("pack entry size varint is too long")
		}
	}
	return typ, size, off, nil
}

// inflateOne inflates a single zlib stream starting at off and reports the
// bytes and the offset just past the stream. The byte reader makes the
// inflater consume exactly the stream's bytes.
func inflateOne(b []byte, off int) ([]byte, int, error) {
	if off >= len(b) {
		return nil, 0, errors.New("zlib stream is truncated")
	}
	r := bytes.NewReader(b[off:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %w", off, err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %w", off, err)
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(b[off:]) - r.Len()
	return data, off + consumed, nil
}

// applyDelta applies a git delta instruction stream to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int, error) {
		v, shift := 0, uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			c := delta[off]
			off++
			v += int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", srcSize, len(base))
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize, shift := 0, 0, uint(0)
			for bit := uint(0); bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := uint(4); bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || len(out)+copySize > tgtSize {
				return nil, errors.New("delta copy is out of range")
			}
			out = append(out, base[copyOff:copyOff+copySize]...)
		} else if op != 0 {
			n := int(op & 0x7f)
			if off+n > len(delta) || len(out)+n > tgtSize {
				return nil, errors.New("delta insert is truncated")
			}
			out = append(out, delta[off:off+n]...)
			off += n
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if len(out) != tgtSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", len(out), tgtSize)
	}
	return out, nil
}

// ReadPack parses a packfile, resolving deltas against earlier entries in
// the same pack or objects the lookup finds. It returns the entries in
// pack order with their computed ids. A malformed pack fails before any
// entry is returned, so callers can refuse the whole push atomically.
func ReadPack(pack []byte, lookup ObjLookup) ([]*PackEntry, error) {
	if len(pack) < 12+20 {
		return nil, errors.New("pack is shorter than its header and trailer")
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack has a bad header")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	// Trailer check first: a bad checksum is a corrupt pack, full stop.
	want := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(want[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack trailer does not match its bytes")
	}
	bodyEnd := len(pack) - 20
	off := 12
	entries := make([]*PackEntry, 0, count)
	byOffset := make(map[int]*PackEntry)
	for i := 0; i < count; i++ {
		if off >= bodyEnd {
			return nil, errors.New("pack is truncated")
		}
		start := off
		typ, size, next, err := readPackEntryHeader(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		switch typ {
		case 1, 2, 3, 4:
			data, n, err := inflateOne(pack, off)
			if err != nil {
				return nil, err
			}
			if len(data) != size {
				return nil, fmt.Errorf("pack object inflates to %d bytes, header says %d", len(data), size)
			}
			off = n
			t := packCodeType[typ]
			ent := &PackEntry{OID: HashObject(t, data), Type: t, Data: data}
			entries = append(entries, ent)
			byOffset[start] = ent
		case 6: // OFS_DELTA
			if off >= bodyEnd {
				return nil, errors.New("ofs-delta has no base offset")
			}
			c := pack[off]
			off++
			baseOff := int(c & 0x7f)
			for c&0x80 != 0 {
				if off >= bodyEnd {
					return nil, errors.New("ofs-delta base offset is truncated")
				}
				c = pack[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(c&0x7f)
			}
			base := byOffset[start-baseOff]
			if base == nil {
				return nil, errors.New("delta base is absent from the pack")
			}
			delta, n, err := inflateOne(pack, off)
			if err != nil {
				return nil, err
			}
			// A delta entry's header size is the size of the inflated
			// delta data, not of the resolved object (git's pack
			// format): check the delta here, and let applyDelta
			// enforce the resolved size from the delta's own target
			// varint.
			if len(delta) != size {
				return nil, fmt.Errorf("delta inflates to %d bytes, header says %d", len(delta), size)
			}
			off = n
			data, err := applyDelta(base.Data, delta)
			if err != nil {
				return nil, err
			}
			ent := &PackEntry{OID: HashObject(base.Type, data), Type: base.Type, Data: data}
			entries = append(entries, ent)
			byOffset[start] = ent
		case 7: // REF_DELTA
			if off+20 > bodyEnd {
				return nil, errors.New("ref-delta has no base id")
			}
			baseOID := hex.EncodeToString(pack[off : off+20])
			off += 20
			var base *Obj
			for _, e := range entries {
				if e.OID == baseOID {
					base = &Obj{Type: e.Type, Data: e.Data}
					break
				}
			}
			if base == nil && lookup != nil {
				if o, ok := lookup(baseOID); ok {
					base = o
				}
			}
			if base == nil {
				return nil, errors.New("delta base is absent")
			}
			delta, n, err := inflateOne(pack, off)
			if err != nil {
				return nil, err
			}
			// A delta entry's header size is the size of the inflated
			// delta data, not of the resolved object (git's pack
			// format): check the delta here, and let applyDelta
			// enforce the resolved size from the delta's own target
			// varint.
			if len(delta) != size {
				return nil, fmt.Errorf("delta inflates to %d bytes, header says %d", len(delta), size)
			}
			off = n
			data, err := applyDelta(base.Data, delta)
			if err != nil {
				return nil, err
			}
			ent := &PackEntry{OID: HashObject(base.Type, data), Type: base.Type, Data: data}
			entries = append(entries, ent)
			byOffset[start] = ent
		default:
			return nil, fmt.Errorf("pack entry has unknown type %d", typ)
		}
	}
	if off != bodyEnd {
		return nil, errors.New("pack has trailing bytes before its trailer")
	}
	return entries, nil
}

// ---------------------------------------------------------------- reachability

// Reachable returns every object id reachable from roots: commit -> tree
// and parents, tag -> target, tree -> entries (gitlinks skipped). A missing
// object is an error unless allowMissing.
func Reachable(lookup ObjLookup, roots []string, allowMissing bool) (map[string]bool, error) {
	seen := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] || oid == ZeroOID {
			continue
		}
		seen[oid] = true
		obj, ok := lookup(oid)
		if !ok {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("missing object %s", oid)
		}
		switch obj.Type {
		case "commit":
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, cv.Tree)
			queue = append(queue, cv.Parents...)
		case "tag":
			tv, err := ParseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, tv.Object)
		case "tree":
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, nil
}

// IsAncestor reports whether ancestor is reachable from descendant by
// following commit parents (or is the same commit).
func IsAncestor(lookup ObjLookup, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	if ancestor == ZeroOID || descendant == ZeroOID {
		return false
	}
	seen := make(map[string]bool)
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := lookup(oid)
		if !ok || obj.Type != "commit" {
			continue
		}
		cv, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range cv.Parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// MergeBase returns the first commit reachable from both a and b when
// walking b's ancestry in breadth-first order, or "" when there is none.
func MergeBase(lookup ObjLookup, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ancA, err := Reachable(lookup, []string{a}, true)
	if err != nil {
		return ""
	}
	seen := make(map[string]bool)
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancA[oid] {
			return oid
		}
		obj, ok := lookup(oid)
		if !ok || obj.Type != "commit" {
			continue
		}
		cv, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, cv.Parents...)
	}
	return ""
}

// PeelToCommit follows tag objects until it reaches a commit (or a
// non-tag object), returning that object's id.
func PeelToCommit(lookup ObjLookup, oid string) string {
	seen := make(map[string]bool)
	for {
		if seen[oid] {
			return oid
		}
		seen[oid] = true
		obj, ok := lookup(oid)
		if !ok || obj.Type != "tag" {
			return oid
		}
		tv, err := ParseTag(obj.Data)
		if err != nil {
			return oid
		}
		oid = tv.Object
	}
}

// FlatFiles returns the path -> blob id map of a commit's tree, walking
// subdirectories depth-first.
func FlatFiles(lookup ObjLookup, commitOID string) (map[string]string, error) {
	obj, ok := lookup(commitOID)
	if !ok || obj.Type != "commit" {
		return nil, fmt.Errorf("no commit %s", commitOID)
	}
	cv, err := ParseCommit(obj.Data)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	var walkTree func(treeOID, prefix string) error
	walkTree = func(treeOID, prefix string) error {
		tobj, ok := lookup(treeOID)
		if !ok || tobj.Type != "tree" {
			return fmt.Errorf("no tree %s", treeOID)
		}
		entries, err := ParseTree(tobj.Data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			path := prefix + e.Name
			switch e.Mode {
			case "40000":
				if err := walkTree(e.OID, path+"/"); err != nil {
					return err
				}
			case "160000":
				// gitlink: not a file we hold.
			default:
				out[path] = e.OID
			}
		}
		return nil
	}
	if err := walkTree(cv.Tree, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// BuildTreeFromFiles builds (and stores, via put) the tree objects for a
// flat path -> blob map, returning the root tree id. Empty directories are
// never stored, matching git.
func BuildTreeFromFiles(files map[string]string, put func(o *Obj) string) (string, error) {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	newNode := func() *node { return &node{files: map[string]string{}, dirs: map[string]*node{}} }
	root := newNode()
	for path, oid := range files {
		parts := splitPath(path)
		n := root
		for _, seg := range parts[:len(parts)-1] {
			c, ok := n.dirs[seg]
			if !ok {
				c = newNode()
				n.dirs[seg] = c
			}
			n = c
		}
		n.files[parts[len(parts)-1]] = oid
	}
	var build func(n *node) (string, error)
	build = func(n *node) (string, error) {
		var entries []TreeEntry
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			sub, err := build(child)
			if err != nil {
				return "", err
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: sub})
		}
		data, err := BuildTree(entries)
		if err != nil {
			return "", err
		}
		return put(&Obj{Type: "tree", Data: data}), nil
	}
	return build(root)
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return append(out, p[start:])
}

// FetchPlan is the result of planning one fetch: the objects to send plus
// the shallow-info lines the response must carry.
type FetchPlan struct {
	Objects   []*Obj
	Shallow   []string
	Unshallow []string
}

// PlanFetch computes what a fetch must send. wants are the wanted object
// ids, haves the ids the client already holds, oldShallow the client's
// current shallow boundary, depth the requested depth (0 = full history).
// relative means the depth is relative to the client's current shallow
// boundary (v2 "deepen-relative", or v0 deepen on a shallow client,
// which has no flag); without it, depth is absolute from the want.
// Objects the client already holds are never re-sent; a shallow boundary
// cuts the client's held set off at the boundary commit.
func PlanFetch(lookup ObjLookup, wants, haves, oldShallow []string, depth int, relative bool) (*FetchPlan, error) {
	// Effective depth: when the deepen is relative, the new limit is
	// the client's current depth plus the deepen amount. The client's
	// current depth is the generation of its shallow boundary from
	// the want, plus one.
	effectiveDepth := depth
	if depth > 0 && relative && len(oldShallow) > 0 {
		boundarySet := make(map[string]bool, len(oldShallow))
		for _, o := range oldShallow {
			boundarySet[o] = true
		}
		baseDepth := 0
		type genItem struct {
			oid string
			gen int
		}
		seen := make(map[string]bool)
		gq := []genItem{}
		for _, w := range wants {
			gq = append(gq, genItem{w, 0})
		}
		for len(gq) > 0 {
			it := gq[0]
			gq = gq[1:]
			if seen[it.oid] || it.oid == ZeroOID {
				continue
			}
			seen[it.oid] = true
			if boundarySet[it.oid] && it.gen+1 > baseDepth {
				baseDepth = it.gen + 1
			}
			obj, ok := lookup(it.oid)
			if !ok || obj.Type != "commit" {
				continue
			}
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, p := range cv.Parents {
				gq = append(gq, genItem{p, it.gen + 1})
			}
		}
		if baseDepth > 0 {
			effectiveDepth = baseDepth + depth
		}
	}
	// Held set: everything reachable from haves, stopping at old shallow
	// boundaries (the boundary commit itself and its tree are held, its
	// parents are not).
	held := make(map[string]bool)
	oldShallowSet := make(map[string]bool, len(oldShallow))
	for _, o := range oldShallow {
		oldShallowSet[o] = true
	}
	queue := append([]string(nil), haves...)
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if held[oid] || oid == ZeroOID {
			continue
		}
		obj, ok := lookup(oid)
		if !ok {
			continue
		}
		held[oid] = true
		switch obj.Type {
		case "commit":
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			treeObjs, err := Reachable(lookup, []string{cv.Tree}, true)
			if err != nil {
				return nil, err
			}
			for o := range treeObjs {
				held[o] = true
			}
			if !oldShallowSet[oid] {
				queue = append(queue, cv.Parents...)
			}
		case "tag":
			tv, err := ParseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, tv.Object)
		case "tree":
			treeObjs, err := Reachable(lookup, []string{oid}, true)
			if err != nil {
				return nil, err
			}
			for o := range treeObjs {
				held[o] = true
			}
		}
	}

	// Included set: walk from wants, honouring the effective depth. A
	// commit at generation >= effective depth is not included at all.
	included := make(map[string]bool)
	includedCommits := make(map[string]bool)
	includedGen := make(map[string]int)
	type item struct {
		oid string
		gen int
	}
	q := []item{}
	for _, w := range wants {
		q = append(q, item{w, 0})
	}
	for len(q) > 0 {
		it := q[0]
		q = q[1:]
		if included[it.oid] || it.oid == ZeroOID {
			continue
		}
		if effectiveDepth > 0 && it.gen >= effectiveDepth {
			continue
		}
		obj, ok := lookup(it.oid)
		if !ok {
			continue
		}
		included[it.oid] = true
		includedGen[it.oid] = it.gen
		switch obj.Type {
		case "commit":
			includedCommits[it.oid] = true
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			treeObjs, err := Reachable(lookup, []string{cv.Tree}, true)
			if err != nil {
				return nil, err
			}
			for o := range treeObjs {
				included[o] = true
			}
			for _, p := range cv.Parents {
				q = append(q, item{p, it.gen + 1})
			}
		case "tag":
			tv, err := ParseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			q = append(q, item{tv.Object, it.gen})
		}
	}

	plan := &FetchPlan{}
	type fetchObj struct {
		oid string
		obj *Obj
	}
	var ordered []fetchObj
	for oid := range included {
		if held[oid] {
			continue
		}
		obj, ok := lookup(oid)
		if !ok {
			continue
		}
		ordered = append(ordered, fetchObj{oid, &Obj{Type: obj.Type, Data: obj.Data}})
	}
	// Deterministic pack order: sort by id so the same fetch always
	// produces the same bytes. The id is already known (it is the map
	// key), so sort on it directly: hashing inside the comparator
	// would re-hash every object on every comparison.
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].oid < ordered[j].oid })
	for _, fo := range ordered {
		plan.Objects = append(plan.Objects, fo.obj)
	}
	if effectiveDepth > 0 {
		// New boundary: an included commit at the depth limit (its
		// parents, if any, were cut off), or an included commit whose
		// parents were not all included. A root commit at the limit
		// is a boundary too: the client's history ends there.
		for oid := range includedCommits {
			obj, ok := lookup(oid)
			if !ok || obj.Type != "commit" {
				continue
			}
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			boundary := includedGen[oid] == effectiveDepth-1
			if !boundary {
				for _, p := range cv.Parents {
					if !includedCommits[p] {
						boundary = true
					}
				}
			}
			if boundary {
				plan.Shallow = append(plan.Shallow, oid)
			}
		}
		sort.Strings(plan.Shallow)
		// Resolved boundaries: an old boundary whose parents are now all
		// included (or held) is no longer shallow.
		for _, b := range oldShallow {
			obj, ok := lookup(b)
			if !ok || obj.Type != "commit" {
				continue
			}
			cv, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			resolved := true
			for _, p := range cv.Parents {
				if !included[p] && !held[p] {
					resolved = false
				}
			}
			if resolved && (included[b] || held[b]) {
				plan.Unshallow = append(plan.Unshallow, b)
			}
		}
		sort.Strings(plan.Unshallow)
	}
	return plan, nil
}

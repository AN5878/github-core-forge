// Git's object model, packfile read/write and pkt-line framing, on the Go
// standard library only. No deltas are ever written; deltas are resolved on read
// (a REF_DELTA base may live in the repo's own store).

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strconv"
)

// GitObjectType is one of commit, tree, blob, tag.
type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

// GitObject is one stored object.
type GitObject struct {
	Type GitObjectType
	Data []byte
}

const ZeroOID = "0000000000000000000000000000000000000000"

func isObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

var typeCode = map[GitObjectType]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]GitObjectType{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func hashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func putObject(store map[string]*GitObject, typ GitObjectType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := store[oid]; !ok {
		store[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

// ---------------------------------------------------------------- trees

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

// compareTreeNames is git's tree-entry sort: byte order of the name, a directory
// compared as if it ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	left := a.Name
	if a.Mode == "40000" {
		left += "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right += "/"
	}
	return bytes.Compare([]byte(left), []byte(right))
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- commits / tags

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	var message string
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	cv := CommitView{}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		space := bytes.IndexByte(line, ' ')
		var key, value string
		if space < 0 {
			key = string(line)
		} else {
			key = string(line[:space])
			value = string(line[space+1:])
		}
		switch key {
		case "tree":
			cv.Tree = value
		case "parent":
			cv.Parents = append(cv.Parents, value)
		case "author":
			cv.Author = value
		case "committer":
			cv.Committer = value
		}
	}
	cv.Message = message
	return cv
}

// TagView is a parsed tag object.
type TagView struct {
	Object  string
	Type    GitObjectType
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range bytes.Split(data[:headerEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		space := bytes.IndexByte(line, ' ')
		if space >= 0 {
			headers[string(line[:space])] = string(line[space+1:])
		}
	}
	tv := TagView{
		Object: headers["object"],
		Type:   GitObjectType(headers["type"]),
		Tag:    headers["tag"],
		Tagger: headers["tagger"],
	}
	if tv.Type == "" {
		tv.Type = "commit"
	}
	if blank >= 0 {
		tv.Message = string(data[blank+2:])
	}
	return tv
}

func buildCommitText(tree string, parents []string, author, committer, message string) []byte {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + author + "\n")
	buf.WriteString("committer " + committer + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

// ---------------------------------------------------------------- reachability

// ObjectLookup resolves an oid to its object.
type ObjectLookup func(oid string) *GitObject

// collectReachable returns every object reachable from roots
// (commit -> tree + parents, tree -> entries, tag -> target), in first-visit
// order so callers that iterate it stay deterministic across runs.
func collectReachable(roots []string, lookup ObjectLookup) []string {
	seen := map[string]bool{}
	order := []string{}
	var queue []string
	for _, root := range roots {
		if isObjectID(root) {
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
		switch object.Type {
		case "commit":
			commit := parseCommit(object.Data)
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case "tag":
			queue = append(queue, parseTag(object.Data).Object)
		case "tree":
			for _, e := range parseTree(object.Data) {
				if e.Mode != "160000" {
					queue = append(queue, e.OID)
				}
			}
		}
	}
	return order
}

// isAncestor is true when ancestor is oid or reachable from it through commit parents.
func isAncestor(ancestor, oid string, lookup ObjectLookup) bool {
	if ancestor == oid {
		return true
	}
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		object := lookup(current)
		if object == nil || object.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// findMergeBase is the first commit reachable from both a and b
// (best-effort merge base, BFS by distance).
func findMergeBase(a, b string, lookup ObjectLookup) string {
	if a == b {
		return a
	}
	distA := map[string]int{}
	type item struct {
		oid string
		d   int
	}
	queueA := []item{{a, 0}}
	headA := 0
	for headA < len(queueA) {
		cur := queueA[headA]
		headA++
		if _, ok := distA[cur.oid]; ok {
			continue
		}
		distA[cur.oid] = cur.d
		object := lookup(cur.oid)
		if object == nil || object.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queueA = append(queueA, item{parent, cur.d + 1})
		}
	}
	queueB := []item{{b, 0}}
	headB := 0
	seenB := map[string]bool{}
	best := ""
	bestScore := int(^uint(0) >> 1)
	for headB < len(queueB) {
		cur := queueB[headB]
		headB++
		if seenB[cur.oid] {
			continue
		}
		seenB[cur.oid] = true
		if da, ok := distA[cur.oid]; ok && da+cur.d < bestScore {
			bestScore = da + cur.d
			best = cur.oid
		}
		object := lookup(cur.oid)
		if object == nil || object.Type != "commit" {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queueB = append(queueB, item{parent, cur.d + 1})
		}
	}
	return best
}

// FileEntry is one path in a flattened tree.
type FileEntry struct {
	OID  string
	Mode string
}

// flattenTree flattens a tree into path -> entry, recursively.
func flattenTree(rootTree string, lookup ObjectLookup) map[string]*FileEntry {
	out := map[string]*FileEntry{}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		object := lookup(treeOID)
		if object == nil || object.Type != "tree" {
			return
		}
		for _, e := range parseTree(object.Data) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = &FileEntry{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(rootTree, "")
	return out
}

// buildTreeFromFiles builds (and stores) the tree objects for a flat file map
// and returns the root tree oid. Directories are rebuilt from their children;
// unchanged subtrees keep their oid.
func buildTreeFromFiles(files map[string]*FileEntry, store map[string]*GitObject) string {
	type node struct {
		files map[string]*FileEntry
		dirs  map[string]*node
	}
	makeNode := func() *node { return &node{files: map[string]*FileEntry{}, dirs: map[string]*node{}} }
	root := makeNode()
	// Deterministic build order: iterate paths sorted so the store fills
	// identically on every run.
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := files[path]
		segments := splitPath(path)
		n := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			child, ok := n.dirs[name]
			if !ok {
				child = makeNode()
				n.dirs[name] = child
			}
			n = child
		}
		n.files[segments[len(segments)-1]] = entry
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := []TreeEntry{}
		for name, e := range n.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return putObject(store, "tree", buildTree(entries))
	}
	return build(root)
}

func splitPath(path string) []string {
	var out []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			out = append(out, path[start:i])
			start = i + 1
		}
	}
	return append(out, path[start:])
}

// ---------------------------------------------------------------- packfile read

func readVarintSize(buffer []byte, start int) (size int, next int, first byte) {
	b := buffer[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	offset := start + 1
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, offset, first
}

func readOfsDeltaBase(buffer []byte, start int) (baseOffset int, next int) {
	b := buffer[start]
	offset := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		b = buffer[offset]
		offset++
		value = ((value + 1) * 128) + int(b&0x7f)
	}
	return value, offset
}

// oneByteReader feeds bytes one at a time so the flate reader can never pull
// past the end of the zlib stream: the number of bytes pulled is exactly the
// stream length (verified: pulled == stream length, adler matches).
type oneByteReader struct {
	b []byte
	n int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.n >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.n]
	r.n++
	return 1, nil
}

// inflateAt inflates one zlib stream starting at offset (bounded by end) and
// returns the data and the offset just past the stream.
func inflateAt(buffer []byte, offset, end int) (data []byte, next int, err error) {
	cr := &oneByteReader{b: buffer[offset:end]}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(cr, hdr); err != nil {
		return nil, 0, err
	}
	fr := flate.NewReader(cr)
	out, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return nil, 0, err
	}
	ad := make([]byte, 4)
	if _, err := io.ReadFull(cr, ad); err != nil {
		return nil, 0, err
	}
	if adler32.Checksum(out) != binary.BigEndian.Uint32(ad) {
		return nil, 0, errors.New("zlib adler32 mismatch")
	}
	return out, offset + cr.n, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readSize := func() int {
		value := 0
		shift := uint(0)
		for {
			b := delta[offset]
			offset++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	sourceSize := readSize()
	targetSize := readSize()
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		op := delta[offset]
		offset++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copyOffset += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copySize += int(delta[offset]) << shift
					offset++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			copy(out[written:], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			copy(out[written:], delta[offset:offset+literal])
			offset += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset       int
	typeCode     int
	size         int
	data         []byte
	baseOffset   int
	baseOID      string
	resolvedType GitObjectType
	resolved     []byte
	oid          string
	hasResolved  bool
}

// readPack reads a packfile (v2/v3), resolving OFS_DELTA and REF_DELTA;
// resolveBase supplies thin-pack bases.
func readPack(pack []byte, resolveBase ObjectLookup) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := binary.BigEndian.Uint32(pack[8:12])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	h := sha1.New()
	h.Write(pack[:trailerStart])
	if hex.EncodeToString(h.Sum(nil)) != trailer {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	raw := []*rawPackEntry{}
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for index := uint32(0); index < declaredCount; index++ {
		entryOffset := offset
		size, next, first := readVarintSize(pack, offset)
		offset = next
		tc := int((first >> 4) & 0x07)
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, nextOff := readOfsDeltaBase(pack, offset)
			offset = nextOff
			entry.baseOffset = entryOffset - base
		} else if tc == 7 {
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if _, ok := codeType[tc]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, tc)
		}
		inflated, nextOff, err := inflateAt(pack, offset, trailerStart)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		offset = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}

	objects := map[string]*GitObject{}
	resolvedByOID := map[string]*rawPackEntry{}
	resolving := map[int]bool{}
	var resolve func(entry *rawPackEntry) (GitObjectType, []byte, string, error)
	resolve = func(entry *rawPackEntry) (GitObjectType, []byte, string, error) {
		if entry.hasResolved {
			return entry.resolvedType, entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return "", nil, "", errors.New("delta cycle in pack")
		}
		resolving[entry.offset] = true
		var typ GitObjectType
		var data []byte
		var err error
		if entry.typeCode == 6 {
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return "", nil, "", errors.New("ofs-delta base is not in the pack")
			}
			var baseType GitObjectType
			var baseData []byte
			baseType, baseData, _, err = resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = applyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.typeCode == 7 {
			wanted := entry.baseOID
			if baseEntry, ok := resolvedByOID[wanted]; ok {
				var baseType GitObjectType
				var baseData []byte
				baseType, baseData, _, err = resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = baseType
				data, err = applyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			} else {
				external := resolveBase(wanted)
				if external == nil {
					return "", nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", wanted)
				}
				typ = external.Type
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resolved = data
		entry.resolvedType = typ
		entry.oid = oid
		entry.hasResolved = true
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return typ, data, oid, nil
	}
	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = &GitObject{Type: typ, Data: data}
		}
	}
	return objects, nil
}

// ---------------------------------------------------------------- packfile write

func writePackObjectHeader(typ GitObjectType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := byte((code << 4) | (remaining & 0x0f))
	remaining /= 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	out = append(out, first)
	return out
}

// packObject is one object to write, with its oid known so the writer does not
// re-hash every object.
type packObject struct {
	Type GitObjectType
	Data []byte
	OID  string
}

// writePack builds a packfile from plain objects (no deltas), deduplicated by oid.
func writePack(objects []packObject) []byte {
	seen := map[string]*packObject{}
	order := []*packObject{}
	for i := range objects {
		o := &objects[i]
		oid := o.OID
		if oid == "" {
			oid = hashObject(o.Type, o.Data)
		}
		if _, ok := seen[oid]; !ok {
			seen[oid] = o
			order = append(order, o)
		}
	}
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(order)))
	buf.Write(header)
	for _, o := range order {
		buf.Write(writePackObjectHeader(o.Type, len(o.Data)))
		zw, _ := zlib.NewWriterLevel(&buf, 1)
		zw.Write(o.Data)
		zw.Close()
	}
	body := buf.Bytes()
	h := sha1.New()
	h.Write(body)
	return append(body, h.Sum(nil)...)
}

// ---------------------------------------------------------------- pkt-line

func encodePktLine(payload []byte) []byte {
	header := make([]byte, 4)
	copy(header, []byte(fmt.Sprintf("%04x", len(payload)+4)))
	return append(header, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// encodePktLines encodes items: a []byte or string becomes a data line,
// "flush" a flush packet, "delim" a delim packet.
func encodePktLines(items []interface{}) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				buf.Write(pktFlush)
			} else if v == "delim" {
				buf.Write(pktDelim)
			} else {
				buf.Write(encodePktLine([]byte(v)))
			}
		case []byte:
			buf.Write(encodePktLine(v))
		}
	}
	return buf.Bytes()
}

// GitPacket is one decoded pkt-line.
type GitPacket struct {
	Kind    string // "data", "flush", "delim", "response-end"
	Payload []byte
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodePktLines decodes a complete buffer into pkt-line packets.
func decodePktLines(data []byte) ([]GitPacket, error) {
	packets := []GitPacket{}
	offset := 0
	for offset < len(data) {
		if offset+4 > len(data) {
			return nil, errors.New("truncated pkt-line")
		}
		header := string(data[offset : offset+4])
		if !isHex4(header) {
			return nil, fmt.Errorf("invalid pkt-line length %q", header)
		}
		var length int
		n, _ := strconv.ParseUint(header, 16, 32)
		length = int(n)
		if length == 0 {
			packets = append(packets, GitPacket{Kind: "flush"})
			offset += 4
		} else if length == 1 {
			packets = append(packets, GitPacket{Kind: "delim"})
			offset += 4
		} else if length == 2 {
			packets = append(packets, GitPacket{Kind: "response-end"})
			offset += 4
		} else if length == 3 || length > 65520 {
			return nil, fmt.Errorf("invalid pkt-line length %d", length)
		} else {
			if offset+length > len(data) {
				return nil, errors.New("truncated pkt-line")
			}
			packets = append(packets, GitPacket{Kind: "data", Payload: data[offset+4 : offset+length]})
			offset += length
		}
	}
	return packets, nil
}

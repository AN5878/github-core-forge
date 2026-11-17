// git smart HTTP: info/refs (v2 and v0 advertisements), git-upload-pack
// (protocol v2 ls-refs/fetch, v0 fetch) and git-receive-pack (v0, report-status
// with side-band-64k). Packfiles are plain objects (no deltas); the trailer
// checksum is SHA-1 over the pack bytes.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const agent = "forge/1.0"

func sendGit(status int, body []byte, contentType string) Response {
	return textResponse(status, body, contentType, nil)
}

func gitNotFound() Response {
	return jsonResponse(404, map[string]interface{}{"message": "Not Found"}, nil)
}

func gitBadRequest(message string) Response {
	return jsonResponse(400, map[string]interface{}{"message": message}, nil)
}

// RefLine is one ref with its peeled target for annotated tags.
type RefLine struct {
	Name   string
	OID    string
	Peeled string
}

// listRefs returns all refs of a repo, sorted, with peeled targets for
// annotated tags.
func listRefs(repo *Repo) []RefLine {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []RefLine{}
	for _, name := range names {
		oid := repo.Refs[name]
		entry := RefLine{Name: name, OID: oid}
		if strings.HasPrefix(name, "refs/tags/") {
			if object := repo.Objects[oid]; object != nil && object.Type == "tag" {
				entry.Peeled = parseTag(object.Data).Object
			}
		}
		out = append(out, entry)
	}
	return out
}

func headOID(repo *Repo) string {
	if oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		return oid
	}
	return ZeroOID
}

// v0Advertisement builds the v0 advertisement body for a service, with the
// `# service=` preamble.
func v0Advertisement(repo *Repo, service string) []byte {
	defaultBranch := repo.DefaultBranch
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs symref=HEAD:refs/heads/" + defaultBranch + " agent=" + agent + " ofs-delta"
	} else {
		caps = "side-band-64k symref=HEAD:refs/heads/" + defaultBranch + " agent=" + agent + " shallow ofs-delta thin-pack"
	}
	var buf bytes.Buffer
	buf.Write(encodePktLine([]byte("# service=" + service + "\n")))
	buf.Write(pktFlush)
	refs := listRefs(repo)
	if len(refs) == 0 {
		buf.Write(encodePktLine([]byte(ZeroOID + " capabilities^{}\x00" + caps + "\n")))
	} else {
		buf.Write(encodePktLine([]byte(headOID(repo) + " HEAD\x00" + caps + "\n")))
		for _, ref := range refs {
			buf.Write(encodePktLine([]byte(ref.OID + " " + ref.Name + "\n")))
			if ref.Peeled != "" {
				buf.Write(encodePktLine([]byte(ref.Peeled + " " + ref.Name + "^{}\n")))
			}
		}
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// v2Advertisement builds the v2 capability advertisement body for upload-pack.
func v2Advertisement() []byte {
	var buf bytes.Buffer
	buf.Write(encodePktLine([]byte("# service=git-upload-pack\n")))
	buf.Write(pktFlush)
	buf.Write(encodePktLine([]byte("version 2\n")))
	buf.Write(encodePktLine([]byte("agent=" + agent + "\n")))
	buf.Write(encodePktLine([]byte("ls-refs=unborn\n")))
	buf.Write(encodePktLine([]byte("fetch=shallow filter\n")))
	buf.Write(encodePktLine([]byte("object-format=sha1\n")))
	buf.Write(pktFlush)
	return buf.Bytes()
}

// sidebandPackets frames data as band-1 sideband packets (<= 65515 bytes each).
func sidebandPackets(data []byte) [][]byte {
	out := [][]byte{}
	const chunk = 65515
	if len(data) == 0 {
		out = append(out, encodePktLine(append([]byte{1}, []byte{}...)))
		return out
	}
	for offset := 0; offset < len(data); offset += chunk {
		end := offset + chunk
		if end > len(data) {
			end = len(data)
		}
		packet := append([]byte{1}, data[offset:end]...)
		out = append(out, encodePktLine(packet))
	}
	return out
}

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.*)$`)

func handleGit(req *Request, ctx *Ctx) Response {
	match := gitRouteRe.FindStringSubmatch(req.Path)
	if match == nil {
		return gitNotFound()
	}
	owner, _ := urlPathUnescape(match[1])
	repoName, _ := urlPathUnescape(match[2])
	suffix := match[3]

	if !checkAuth(req, ctx) {
		return jsonResponse(401, map[string]interface{}{"message": "Unauthorized"}, nil)
	}

	repo := ctx.Forge.getRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}

	if req.Method == "GET" && suffix == "info/refs" {
		service := req.Query.Get("service")
		if service == "git-upload-pack" {
			v2 := req.Headers["git-protocol"] == "version=2"
			if v2 {
				return sendGit(200, v2Advertisement(), "application/x-git-upload-pack-advertisement")
			}
			return sendGit(200, v0Advertisement(repo, "git-upload-pack"), "application/x-git-upload-pack-advertisement")
		}
		if service == "git-receive-pack" {
			return sendGit(200, v0Advertisement(repo, "git-receive-pack"), "application/x-git-receive-pack-advertisement")
		}
		return gitNotFound()
	}

	if req.Method == "POST" && suffix == "git-upload-pack" {
		if req.Headers["git-protocol"] == "version=2" {
			return handleUploadPackV2(req, ctx, repo)
		}
		return handleUploadPackV0(req, ctx, repo)
	}

	if req.Method == "POST" && suffix == "git-receive-pack" {
		return handleReceivePack(req, ctx, repo)
	}

	return gitNotFound()
}

// ------------------------------------------------------------------ ls-refs / fetch (v2)

// FetchRequest is one parsed fetch request (v2 or v0).
type FetchRequest struct {
	Wants   []string
	Haves   []string
	Shallow []string
	Depth   int  // -1 means no deepen
	HasDepth bool
	Filter  string
}

func parseV2Command(body []byte) (command string, args []string, err error) {
	packets, err := decodePktLines(body)
	if err != nil {
		return "", nil, err
	}
	afterDelim := false
	for _, packet := range packets {
		if packet.Kind == "delim" {
			afterDelim = true
			continue
		}
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if !afterDelim {
			if strings.HasPrefix(line, "command=") {
				command = line[len("command="):]
			}
		} else {
			args = append(args, line)
		}
	}
	return command, args, nil
}

func lsRefsResponse(repo *Repo, args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	prefixes := []string{}
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[len("ref-prefix "):])
		}
	}
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
	lines := []string{}
	defaultBranch := repo.DefaultBranch
	head := headOID(repo)
	if len(prefixes) == 0 {
		if head != ZeroOID {
			line := head + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + defaultBranch
			}
			lines = append(lines, line+"\n")
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + defaultBranch
			}
			lines = append(lines, line+"\n")
		}
	}
	for _, ref := range listRefs(repo) {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if peel && ref.Peeled != "" {
			line += " peeled:" + ref.Peeled
		}
		lines = append(lines, line+"\n")
	}
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(encodePktLine([]byte(line)))
	}
	buf.Write(pktFlush)
	return buf.Bytes()
}

// computeFetch is the fetch computation shared by v2 and v0: which objects to
// send, the new shallow boundary and the unshallowed commits, and the common
// haves to ACK.
func computeFetch(repo *Repo, request FetchRequest) (objects []packObject, shallow, unshallow, acks []string) {
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	wants := []string{}
	for _, oid := range request.Wants {
		if isObjectID(oid) {
			wants = append(wants, oid)
		}
	}
	depth := -1
	if request.HasDepth {
		depth = request.Depth
	}

	// Generations from the wants (0 = the wants themselves). Index-based queue.
	gen := map[string]int{}
	genOrder := []string{}
	type queueItem struct {
		oid string
		g   int
	}
	queue := []queueItem{}
	for _, oid := range wants {
		queue = append(queue, queueItem{oid, 0})
	}
	queueHead := 0
	sentCommits := map[string]bool{}
	sentOrder := []string{}
	for queueHead < len(queue) {
		item := queue[queueHead]
		queueHead++
		oid, g := item.oid, item.g
		if _, ok := gen[oid]; ok {
			continue
		}
		object := lookup(oid)
		if object == nil {
			continue
		}
		if object.Type == "tag" {
			gen[oid] = g
			genOrder = append(genOrder, oid)
			target := parseTag(object.Data).Object
			queue = append(queue, queueItem{target, g})
			continue
		}
		if object.Type != "commit" {
			gen[oid] = g
			genOrder = append(genOrder, oid)
			continue
		}
		gen[oid] = g
		genOrder = append(genOrder, oid)
		if depth >= 0 && g >= depth {
			continue
		}
		if !sentCommits[oid] {
			sentCommits[oid] = true
			sentOrder = append(sentOrder, oid)
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queue = append(queue, queueItem{parent, g + 1})
		}
	}

	// Objects the client already holds: the haves' closure, stopping at the
	// known shallow boundaries (the client does not hold past them).
	boundary := map[string]bool{}
	for _, oid := range request.Shallow {
		boundary[oid] = true
	}
	held := map[string]bool{}
	heldQueue := append([]string(nil), request.Haves...)
	for len(heldQueue) > 0 {
		oid := heldQueue[len(heldQueue)-1]
		heldQueue = heldQueue[:len(heldQueue)-1]
		if held[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			continue
		}
		held[oid] = true
		switch object.Type {
		case "commit":
			commit := parseCommit(object.Data)
			heldQueue = append(heldQueue, commit.Tree)
			if !boundary[oid] {
				heldQueue = append(heldQueue, commit.Parents...)
			}
		case "tag":
			heldQueue = append(heldQueue, parseTag(object.Data).Object)
		case "tree":
			for _, e := range parseTree(object.Data) {
				heldQueue = append(heldQueue, e.OID)
			}
		}
	}

	// Everything reachable from the sent commits, minus what the client holds.
	wanted := map[string]bool{}
	wantedOrder := []string{}
	addWanted := func(oid string) {
		if !wanted[oid] {
			wanted[oid] = true
			wantedOrder = append(wantedOrder, oid)
		}
	}
	for _, oid := range genOrder {
		g := gen[oid]
		if depth >= 0 && g >= depth {
			continue
		}
		object := lookup(oid)
		if object == nil {
			continue
		}
		switch object.Type {
		case "commit":
			if !sentCommits[oid] {
				continue
			}
			addWanted(oid)
			for _, reachable := range collectReachable([]string{parseCommit(object.Data).Tree}, lookup) {
				addWanted(reachable)
			}
		case "tag":
			addWanted(oid)
		default:
			for _, reachable := range collectReachable([]string{oid}, lookup) {
				addWanted(reachable)
			}
		}
	}
	objects = []packObject{}
	for _, oid := range wantedOrder {
		if held[oid] {
			continue
		}
		if object := lookup(oid); object != nil {
			objects = append(objects, packObject{Type: object.Type, Data: object.Data, OID: oid})
		}
	}

	// Shallow boundary: sent commits with an unsent parent (or at the depth cut).
	shallow = []string{}
	unshallow = []string{}
	if depth >= 0 {
		for _, oid := range sentOrder {
			object := lookup(oid)
			if object == nil || object.Type != "commit" {
				continue
			}
			parents := parseCommit(object.Data).Parents
			hasUnsentParent := false
			for _, parent := range parents {
				if !sentCommits[parent] {
					hasUnsentParent = true
					break
				}
			}
			if hasUnsentParent {
				shallow = append(shallow, oid)
			}
		}
		for _, oid := range request.Shallow {
			if sentCommits[oid] && !contains(shallow, oid) {
				unshallow = append(unshallow, oid)
			}
		}
	} else if len(request.Shallow) > 0 {
		for _, oid := range request.Shallow {
			if sentCommits[oid] {
				unshallow = append(unshallow, oid)
			}
		}
	}

	// ACKs: haves that are ancestors of (or equal to) a want.
	acks = []string{}
	for _, have := range request.Haves {
		if !isObjectID(have) {
			continue
		}
		for _, want := range wants {
			if isAncestor(have, want, lookup) {
				acks = append(acks, have)
				break
			}
		}
	}
	return objects, shallow, unshallow, acks
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ caches

// cachedFetch is one computed fetch: the pack plus the shallow/unshallow/ack
// lists, and (once built) the whole framed response for v2 and for v0.
type cachedFetch struct {
	pack      []byte
	shallow   []string
	unshallow []string
	acks      []string
}

// A cachedFetch is immutable once built: the pack and the shallow/ack lists
// are derived from the object store at one size, and never mutated, so any
// number of concurrent readers can share one entry. (The v2/v0 response
// framing is rebuilt per request from the entry — cheap, and it keeps the
// entry race-free.)

type fetchSlot struct {
	size    int
	entries sync.Map // fetch key -> *cachedFetch
}

// fetchCache maps a repo to its fetch slot. A sync.Map so concurrent read
// handlers can look up (and install) slots without a global lock. A slot is
// valid only while its size matches the repo's object count; a writer can
// only add objects, so a size mismatch means the slot is stale and is
// replaced (Store overwrites). The size cannot change while a read lock is
// held, so a slot built inside fetchPack stays valid for that call.
var fetchCache sync.Map // *Repo -> *fetchSlot

func fetchCacheKey(request FetchRequest) string {
	raw, _ := json.Marshal([]interface{}{request.Wants, request.Haves, request.Shallow, request.Depth, request.Filter})
	return string(raw)
}

func buildFetchEntry(repo *Repo, request FetchRequest) *cachedFetch {
	objects, shallow, unshallow, acks := computeFetch(repo, request)
	return &cachedFetch{pack: writePack(objects), shallow: shallow, unshallow: unshallow, acks: acks}
}

// fetchPack returns the cached fetch computation for an identical upload-pack
// request on an unchanged object store. The store size in the slot
// invalidates it: any object added to the store changes the size, so a cached
// pack can never be stale. The cache holds only derived bytes, never state,
// so live and replay answers agree. Concurrent readers may compute the same
// entry twice (LoadOrStore keeps one); the bytes are deterministic, so the
// duplicate is harmless.
func fetchPack(repo *Repo, request FetchRequest) *cachedFetch {
	key := fetchCacheKey(request)
	size := len(repo.Objects)
	if v, ok := fetchCache.Load(repo); ok {
		slot := v.(*fetchSlot)
		if slot.size == size {
			if hit, ok := slot.entries.Load(key); ok {
				return hit.(*cachedFetch)
			}
			entry := buildFetchEntry(repo, request)
			actual, _ := slot.entries.LoadOrStore(key, entry)
			return actual.(*cachedFetch)
		}
	}
	slot := &fetchSlot{size: size}
	entry := buildFetchEntry(repo, request)
	slot.entries.Store(key, entry)
	fetchCache.Store(repo, slot)
	return entry
}

// ------------------------------------------------------------------ upload-pack v2

func handleUploadPackV2(req *Request, ctx *Ctx, repo *Repo) Response {
	command, args, err := parseV2Command(req.Body)
	if err != nil {
		return gitBadRequest("malformed request")
	}
	if command == "ls-refs" {
		return sendGit(200, lsRefsResponse(repo, args), "application/x-git-upload-pack-result")
	}
	if command != "fetch" {
		return gitBadRequest("unknown command " + command)
	}
	request := FetchRequest{Depth: -1}
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			request.Wants = append(request.Wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			request.Haves = append(request.Haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			request.Shallow = append(request.Shallow, strings.TrimSpace(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			n := 0
			for _, c := range arg[7:] {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			request.Depth = n
			request.HasDepth = true
		case strings.HasPrefix(arg, "filter "):
			request.Filter = arg[7:]
		}
	}
	entry := fetchPack(repo, request)
	var buf bytes.Buffer
	buf.Write(encodePktLine([]byte("acknowledgments\n")))
	if len(entry.acks) > 0 {
		for _, ack := range entry.acks {
			buf.Write(encodePktLine([]byte("ACK " + ack + "\n")))
		}
		buf.Write(encodePktLine([]byte("ready\n")))
	} else {
		buf.Write(encodePktLine([]byte("NAK\n")))
	}
	buf.Write(pktDelim)
	if len(entry.shallow) > 0 || len(entry.unshallow) > 0 {
		buf.Write(encodePktLine([]byte("shallow-info\n")))
		for _, oid := range entry.shallow {
			buf.Write(encodePktLine([]byte("shallow " + oid + "\n")))
		}
		for _, oid := range entry.unshallow {
			buf.Write(encodePktLine([]byte("unshallow " + oid + "\n")))
		}
		buf.Write(pktDelim)
	}
	// listRefs once, indexed by oid: rebuilding it per want is O(wants x refs).
	refsByOID := map[string][]string{}
	for _, ref := range listRefs(repo) {
		refsByOID[ref.OID] = append(refsByOID[ref.OID], ref.Name)
		if ref.Peeled != "" {
			refsByOID[ref.Peeled] = append(refsByOID[ref.Peeled], ref.Name)
		}
	}
	type wantedRef struct {
		oid string
		ref string
	}
	wantedRefs := []wantedRef{}
	for _, want := range request.Wants {
		for _, name := range refsByOID[want] {
			wantedRefs = append(wantedRefs, wantedRef{want, name})
		}
	}
	if len(wantedRefs) > 0 {
		buf.Write(encodePktLine([]byte("wanted-refs\n")))
		for _, wr := range wantedRefs {
			buf.Write(encodePktLine([]byte(wr.oid + " " + wr.ref + "\n")))
		}
		buf.Write(pktDelim)
	}
	buf.Write(encodePktLine([]byte("packfile\n")))
	for _, packet := range sidebandPackets(entry.pack) {
		buf.Write(packet)
	}
	buf.Write(pktFlush)
	return sendGit(200, buf.Bytes(), "application/x-git-upload-pack-result")
}

// ------------------------------------------------------------------ upload-pack v0

func handleUploadPackV0(req *Request, ctx *Ctx, repo *Repo) Response {
	packets, err := decodePktLines(req.Body)
	if err != nil {
		return gitBadRequest("malformed request")
	}
	request := FetchRequest{Depth: -1}
	for _, packet := range packets {
		if packet.Kind == "flush" {
			continue
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := line[5:]
			if space := strings.IndexByte(rest, ' '); space >= 0 {
				rest = rest[:space]
			}
			request.Wants = append(request.Wants, rest)
		case strings.HasPrefix(line, "have "):
			request.Haves = append(request.Haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			request.Shallow = append(request.Shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n := 0
			for _, c := range line[7:] {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
			}
			request.Depth = n
			request.HasDepth = true
		}
	}
	entry := fetchPack(repo, request)
	var buf bytes.Buffer
	if len(entry.acks) > 0 {
		for _, ack := range entry.acks {
			buf.Write(encodePktLine([]byte("ACK " + ack + "\n")))
		}
	} else {
		buf.Write(encodePktLine([]byte("NAK\n")))
	}
	for _, oid := range entry.shallow {
		buf.Write(encodePktLine([]byte("shallow " + oid + "\n")))
	}
	for _, oid := range entry.unshallow {
		buf.Write(encodePktLine([]byte("unshallow " + oid + "\n")))
	}
	for _, packet := range sidebandPackets(entry.pack) {
		buf.Write(packet)
	}
	buf.Write(pktFlush)
	return sendGit(200, buf.Bytes(), "application/x-git-upload-pack-result")
}

// ------------------------------------------------------------------ receive-pack (v0)

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
	caps   string
}

func handleReceivePack(req *Request, ctx *Ctx, repo *Repo) Response {
	body := req.Body
	commands := []pushCommand{}
	offset := 0
	packStart := len(body)
	sideband := false
	for offset+4 <= len(body) {
		header := string(body[offset : offset+4])
		if !isHex4(header) {
			return gitBadRequest("malformed request")
		}
		n, _ := parseHexUint(header)
		length := int(n)
		if length == 0 {
			offset += 4
			packStart = offset
			break
		}
		if length < 4 || offset+length > len(body) {
			return gitBadRequest("malformed request")
		}
		payload := string(body[offset+4 : offset+length])
		nul := strings.IndexByte(payload, 0)
		line := payload
		caps := ""
		if nul >= 0 {
			line = payload[:nul]
			caps = payload[nul+1:]
		}
		line = strings.TrimSuffix(line, "\n")
		caps = strings.TrimSuffix(caps, "\n")
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			return gitBadRequest("malformed request")
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2], caps: caps})
		if strings.Contains(caps, "side-band-64k") {
			sideband = true
		}
		offset += length
	}
	if len(commands) == 0 {
		return gitBadRequest("malformed request")
	}

	// Unpack first: a corrupt pack fails the unpack, never the refs.
	unpackError := ""
	packBytes := body[packStart:]
	if len(packBytes) > 0 {
		objects, err := readPack(packBytes, func(oid string) *GitObject { return repo.Objects[oid] })
		if err != nil {
			unpackError = err.Error()
		} else {
			for oid, object := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = object
				}
			}
		}
	}

	reportLines := []interface{}{}
	if unpackError != "" {
		reportLines = append(reportLines, "unpack "+unpackError+"\n")
	} else {
		reportLines = append(reportLines, "unpack ok\n")
		for _, command := range commands {
			if reason := validatePush(repo, command); reason == "" {
				if command.newOID == ZeroOID {
					delete(repo.Refs, command.ref)
				} else {
					repo.Refs[command.ref] = command.newOID
				}
				reportLines = append(reportLines, "ok "+command.ref+"\n")
			} else {
				reportLines = append(reportLines, "ng "+command.ref+" "+reason+"\n")
			}
		}
	}
	reportLines = append(reportLines, "flush")
	reportStream := encodePktLines(reportLines)
	var buf bytes.Buffer
	if sideband {
		buf.Write(encodePktLine(append([]byte{1}, reportStream...)))
	} else {
		buf.Write(reportStream)
	}
	buf.Write(pktFlush)
	return sendGit(200, buf.Bytes(), "application/x-git-receive-pack-result")
}

// validatePush returns "" when the push command may apply, else the ng reason.
func validatePush(repo *Repo, command pushCommand) string {
	oldOID, newOID, ref := command.oldOID, command.newOID, command.ref
	if !isObjectID(oldOID) || !isObjectID(newOID) {
		return "invalid object id"
	}
	if !strings.HasPrefix(ref, "refs/") {
		return "invalid ref"
	}
	current, hasCurrent := repo.Refs[ref]
	if newOID == ZeroOID {
		// Delete.
		if !hasCurrent {
			return "remote ref does not exist"
		}
		if oldOID != ZeroOID && oldOID != current {
			return "non-fast-forward"
		}
		return ""
	}
	object := repo.Objects[newOID]
	if object == nil {
		return "object not found"
	}
	if strings.HasPrefix(ref, "refs/heads/") && object.Type != "commit" {
		return "not a commit"
	}
	if strings.HasPrefix(ref, "refs/tags/") && object.Type != "commit" && object.Type != "tag" {
		return "not a commit or tag"
	}
	if oldOID == ZeroOID {
		if hasCurrent {
			return "remote ref already exists"
		}
		return ""
	}
	if !hasCurrent {
		return "remote ref does not exist"
	}
	if oldOID != current {
		return "non-fast-forward"
	}
	if newOID == current {
		return ""
	}
	// Fast path: sequential pushes land on the current tip's child, so the new
	// commit's own parents name the current ref directly.
	if newObject := repo.Objects[newOID]; newObject != nil && newObject.Type == "commit" {
		for _, parent := range parseCommit(newObject.Data).Parents {
			if parent == current {
				return ""
			}
		}
	}
	if !isAncestor(current, newOID, func(oid string) *GitObject { return repo.Objects[oid] }) {
		return "non-fast-forward"
	}
	return ""
}

func parseHexUint(s string) (uint64, error) {
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v uint64
		switch {
		case c >= '0' && c <= '9':
			v = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			v = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = uint64(c-'A') + 10
		default:
			return 0, fmt.Errorf("invalid hex %q", s)
		}
		n = n*16 + v
	}
	return n, nil
}

func urlPathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

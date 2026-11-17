// Git smart-HTTP handlers: info/refs advertisements, protocol v2
// ls-refs and fetch, protocol v0 fetch, and receive-pack (push).
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type gitHandlers struct{ store *Store }

func newGitHandlers(s *Store) *gitHandlers { return &gitHandlers{store: s} }

func (h *gitHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /{owner}/{repo}.git/{info/refs|git-upload-pack|git-receive-pack}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 && len(parts) != 4 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	action := parts[2]

	h.store.mu.RLock()
	repo := h.store.GetRepo(owner, repoName)
	h.store.mu.RUnlock()
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}

	switch action {
	case "info":
		if len(parts) == 4 && parts[3] == "refs" && r.Method == http.MethodGet {
			h.infoRefs(w, r, repo)
			return
		}
		writeErr(w, http.StatusNotFound, "Not Found")
	case "git-upload-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		h.uploadPack(w, r, repo)
	case "git-receive-pack":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		h.receivePack(w, r, repo)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// ---------------------------------------------------------------- pkt-lines

// pktLine encodes one pkt-line (4 hex length digits + payload).
func pktLine(payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	copy(out, []byte(fmt.Sprintf("%04x", len(payload)+4)))
	return append(out, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktReader reads pkt-lines one at a time. It returns the payload without
// the length prefix; a flush line returns ("", true, nil) and a delim line
// returns ("", false, nil) with delim=true. A malformed line is an error.
func readPktLine(r io.Reader) (payload []byte, isFlush, isDelim bool, err error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, false, false, err
	}
	n, err := strconv.ParseInt(string(lenBuf[:]), 16, 32)
	if err != nil || n < 0 || n > 65520 {
		return nil, false, false, fmt.Errorf("bad pkt-line length %q", lenBuf)
	}
	switch n {
	case 0:
		return nil, true, false, nil
	case 1:
		return nil, false, true, nil
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, false, false, err
	}
	return buf, false, false, nil
}

// readPktLines reads pkt-lines until a flush, returning the payloads.
func readPktLines(r io.Reader) ([][]byte, error) {
	var out [][]byte
	for {
		payload, isFlush, isDelim, err := readPktLine(r)
		if err != nil {
			return nil, err
		}
		if isFlush {
			return out, nil
		}
		if isDelim {
			continue
		}
		out = append(out, payload)
	}
}

// ---------------------------------------------------------------- advertisements

const capsV0Upload = "side-band-64k thin-pack ofs-delta multi_ack_detailed shallow"
const capsV0Receive = "report-status side-band-64k delete-refs"

// refLines returns the advertisement ref lines for a repository: HEAD
// first (when the default branch has a tip), then heads, then tags, with
// peeled lines after annotated tags.
func (h *gitHandlers) refLines(repo *Repo) []string {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	var lines []string
	tip := repo.Ref("refs/heads/" + repo.DefaultBranch)
	if tip != "" {
		lines = append(lines, tip+" HEAD")
	}
	for _, name := range repo.RefNames() {
		oid := repo.Ref(name)
		lines = append(lines, oid+" "+name)
		if strings.HasPrefix(name, "refs/tags/") {
			if obj, ok := h.store.Lookup(oid); ok && obj.Type == "tag" {
				if tv, err := ParseTag(obj.Data); err == nil {
					peeled := PeelToCommit(h.store.Lookup, tv.Object)
					lines = append(lines, peeled+" "+name+"^{}")
				}
			}
		}
	}
	return lines
}

func (h *gitHandlers) infoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	h.store.mu.RLock()
	tip := repo.Ref("refs/heads/" + repo.DefaultBranch)
	h.store.mu.RUnlock()
	var buf bytes.Buffer
	switch service {
	case "git-upload-pack":
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		buf.Write(pktLine([]byte("# service=git-upload-pack\n")))
		buf.Write(pktFlush)
		if r.Header.Get("Git-Protocol") == "version=2" {
			buf.Write(pktLine([]byte("version 2\n")))
			buf.Write(pktLine([]byte("ls-refs=unborn\n")))
			buf.Write(pktLine([]byte("fetch=shallow\n")))
			buf.Write(pktLine([]byte("object-format=sha1\n")))
			buf.Write(pktFlush)
			break
		}
		if tip == "" {
			buf.Write(pktLine([]byte(ZeroOID + " capabilities^{}\x00" + capsV0Upload + "\n")))
		} else {
			first := tip + " HEAD\x00symref=HEAD:refs/heads/" + repo.DefaultBranch + " " + capsV0Upload + "\n"
			buf.Write(pktLine([]byte(first)))
			for _, line := range h.refLines(repo)[1:] {
				buf.Write(pktLine([]byte(line + "\n")))
			}
		}
		buf.Write(pktFlush)
	case "git-receive-pack":
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		buf.Write(pktLine([]byte("# service=git-receive-pack\n")))
		buf.Write(pktFlush)
		if tip == "" {
			buf.Write(pktLine([]byte(ZeroOID + " capabilities^{}\x00" + capsV0Receive + "\n")))
		} else {
			buf.Write(pktLine([]byte(tip + " HEAD\x00" + capsV0Receive + "\n")))
			for _, line := range h.refLines(repo)[1:] {
				buf.Write(pktLine([]byte(line + "\n")))
			}
		}
		buf.Write(pktFlush)
	default:
		writeErr(w, http.StatusBadRequest, "unknown service")
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------- upload-pack

type fetchRequest struct {
	v2             bool
	command        string
	wants          []string
	haves          []string
	shallow        []string // client's current shallow boundary
	deepen         int
	done           bool // client sent "done" (no more negotiation rounds)
	deepenRelative bool // client sent "deepen-relative": deepen is relative to its current boundary
}

// parseUploadPackRequest parses a v0 upload-pack request body.
func parseUploadPackRequest(body []byte, isV2 bool) (*fetchRequest, error) {
	fr := &fetchRequest{v2: isV2}
	// v0: want lines, shallow/deepen lines, flush, have lines, "done".
	seenFlush := false
	rr := bytes.NewReader(body)
	for {
		payload, isFlush, _, err := readPktLine(rr)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		if isFlush {
			seenFlush = true
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if !seenFlush {
			if strings.HasPrefix(line, "want ") {
				rest := line[5:]
				if i := strings.Index(rest, "\x00"); i >= 0 {
					rest = rest[:i]
				}
				// The first want line carries the capabilities
				// after the OID, space-separated; the OID is the
				// first field only.
				if f := strings.Fields(rest); len(f) > 0 {
					fr.wants = append(fr.wants, f[0])
				}
			} else if strings.HasPrefix(line, "shallow ") {
				fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
			} else if strings.HasPrefix(line, "deepen ") {
				n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
				fr.deepen = n
			}
		} else {
			if strings.HasPrefix(line, "have ") {
				fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
			} else if line == "done" {
				fr.done = true
				break
			}
		}
		if rr.Len() == 0 {
			break
		}
	}
	return fr, nil
}

// parseV2Fetch parses a protocol v2 fetch/ls-refs request.
func parseV2Fetch(body []byte) (*fetchRequest, error) {
	fr := &fetchRequest{v2: true}
	r := bytes.NewReader(body)
	section := 0 // 0 = command, 1 = arguments
	for {
		payload, isFlush, isDelim, err := readPktLine(r)
		if err != nil {
			return nil, err
		}
		if isFlush {
			break
		}
		if isDelim {
			section = 1
			continue
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if section == 0 {
			if strings.HasPrefix(line, "command=") {
				fr.command = line[len("command="):]
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			fr.wants = append(fr.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
			fr.deepen = n
		case line == "deepen-relative":
			fr.deepenRelative = true
		case line == "done":
			fr.done = true
		}
	}
	return fr, nil
}

// lsRefs handles a protocol v2 ls-refs request and writes the response.
func (h *gitHandlers) lsRefs(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	fr, err := parseV2Fetch(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "malformed ls-refs request")
		return
	}
	// Collect arguments from the request body again: peel, symrefs,
	// unborn, ref-prefix.
	args := map[string]bool{}
	var prefixes []string
	{
		rr := bytes.NewReader(body)
		section := 0
		for {
			payload, isFlush, isDelim, err := readPktLine(rr)
			if err != nil {
				break
			}
			if isFlush {
				break
			}
			if isDelim {
				section = 1
				continue
			}
			if section == 1 {
				line := strings.TrimSuffix(string(payload), "\n")
				if strings.HasPrefix(line, "ref-prefix ") {
					prefixes = append(prefixes, strings.TrimSpace(line[len("ref-prefix "):]))
				} else {
					args[line] = true
				}
			}
		}
	}
	_ = fr

	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	var buf bytes.Buffer
	tip := repo.Ref("refs/heads/" + repo.DefaultBranch)
	// HEAD is included when no prefix filter is given, or when a
	// prefix matches "HEAD" (real git asks for "ref-prefix HEAD"
	// explicitly; the kit client never does, so its rule of omitting
	// HEAD under any prefix filter still holds for its requests).
	headWanted := len(prefixes) == 0
	for _, p := range prefixes {
		if strings.HasPrefix("HEAD", p) {
			headWanted = true
		}
	}
	emit := func(oid, name, extra string) {
		line := oid + " " + name + extra + "\n"
		buf.Write(pktLine([]byte(line)))
	}
	if headWanted && tip != "" {
		extra := ""
		if args["symrefs"] {
			extra += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		emit(tip, "HEAD", extra)
	}
	if headWanted && tip == "" && args["unborn"] {
		emit("unborn", "HEAD", " symref-target:refs/heads/"+repo.DefaultBranch)
	}
	for _, name := range repo.RefNames() {
		if len(prefixes) > 0 {
			match := false
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		oid := repo.Ref(name)
		extra := ""
		if strings.HasPrefix(name, "refs/tags/") && args["peel"] {
			if obj, ok := h.store.Lookup(oid); ok && obj.Type == "tag" {
				if tv, err := ParseTag(obj.Data); err == nil {
					peeled := PeelToCommit(h.store.Lookup, tv.Object)
					extra += " peeled:" + peeled
				}
			}
		}
		emit(oid, name, extra)
	}
	buf.Write(pktFlush)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// sidebandPack writes pack bytes as sideband band-1 pkt-lines.
func sidebandPack(w io.Writer, pack []byte) {
	const maxChunk = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := make([]byte, 1, n+1)
		chunk[0] = 1
		chunk = append(chunk, pack[:n]...)
		w.Write(pktLine(chunk))
		pack = pack[n:]
	}
}

// ackLines computes the acknowledgement lines for a fetch: NAK when the
// client holds nothing in common, otherwise one ACK per common have.
func (h *gitHandlers) ackLines(repo *Repo, fr *fetchRequest) []string {
	if len(fr.haves) == 0 {
		return []string{"NAK"}
	}
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	var out []string
	for _, have := range fr.haves {
		for _, want := range fr.wants {
			if IsAncestor(h.store.Lookup, have, want) {
				out = append(out, "ACK "+have)
				break
			}
		}
	}
	if len(out) == 0 {
		return []string{"NAK"}
	}
	return out
}

func (h *gitHandlers) uploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if isV2 {
		// ls-refs is a v2 command on the same endpoint.
		if bytes.Contains(body, []byte("command=ls-refs")) {
			h.lsRefs(w, r, repo, body)
			return
		}
		fr, err := parseV2Fetch(body)
		if err != nil || fr.command != "fetch" {
			writeErr(w, http.StatusBadRequest, "malformed upload-pack request")
			return
		}
		h.doFetch(w, repo, fr, true)
		return
	}
	fr, err := parseUploadPackRequest(body, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "malformed upload-pack request")
		return
	}
	h.doFetch(w, repo, fr, false)
}

// doFetch plans and serves one fetch (v0 or v2).
func (h *gitHandlers) doFetch(w http.ResponseWriter, repo *Repo, fr *fetchRequest, isV2 bool) {
	// v2 deepen is absolute unless the client sent "deepen-relative";
	// v0 deepen on a shallow client is relative by definition (v0 has
	// no deepen-relative flag).
	relative := fr.deepenRelative || (!isV2 && len(fr.shallow) > 0)
	// Pack cache first: the key is exactly the fetch inputs, and object
	// contents are immutable, so a cached pack is provably fresh. The
	// cached entry also carries the shallow-info lines, so a hit needs
	// no re-plan at all. (The relative flag is part of the key: the
	// same deepen means a different plan with and without it.)
	keyParts := []string{repo.key(), "d=" + strconv.Itoa(fr.deepen)}
	if relative {
		keyParts = append(keyParts, "r=1")
	}
	keyParts = append(keyParts, "w="+strings.Join(sortedCopy(fr.wants), ","))
	keyParts = append(keyParts, "h="+strings.Join(sortedCopy(fr.haves), ","))
	keyParts = append(keyParts, "s="+strings.Join(sortedCopy(fr.shallow), ","))
	key := strings.Join(keyParts, "|")
	var pack []byte
	var plan *FetchPlan
	if cached, ok := h.store.PackCacheGet(key); ok {
		pack = cached.pack
		plan = &FetchPlan{Shallow: cached.shallow, Unshallow: cached.unshallow}
	} else {
		h.store.mu.RLock()
		var err error
		plan, err = PlanFetch(h.store.Lookup, fr.wants, fr.haves, fr.shallow, fr.deepen, relative)
		h.store.mu.RUnlock()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not plan fetch")
			return
		}
		h.store.mu.RLock()
		var objs []*Obj
		for _, o := range plan.Objects {
			objs = append(objs, o)
		}
		h.store.mu.RUnlock()
		pack, err = WritePack(objs)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not write pack")
			return
		}
		h.store.PackCachePut(key, packCacheEntry{pack: pack, shallow: plan.Shallow, unshallow: plan.Unshallow})
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(http.StatusOK)
	var buf bytes.Buffer
	if isV2 {
		// Protocol v2 fetch response, matching the reference server.
		// When the request carries haves but no "done", the client
		// is still negotiating: it expects an acknowledgments section
		// (ACK lines + "ready") first, then the shallow-info and
		// packfile sections. When the request carries "done", the
		// client goes straight to the packfile section.
		if len(fr.haves) > 0 && !fr.done {
			buf.Write(pktLine([]byte("acknowledgments\n")))
			for _, a := range h.ackLines(repo, fr) {
				buf.Write(pktLine([]byte(a + "\n")))
			}
			buf.Write(pktLine([]byte("ready\n")))
			buf.Write(pktDelim)
		}
		// A shallow-info section, when present, comes before the
		// packfile section and is separated from it by a delim.
		if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
			buf.Write(pktLine([]byte("shallow-info\n")))
			for _, s := range plan.Shallow {
				buf.Write(pktLine([]byte("shallow " + s + "\n")))
			}
			for _, s := range plan.Unshallow {
				buf.Write(pktLine([]byte("unshallow " + s + "\n")))
			}
			buf.Write(pktDelim)
		}
		buf.Write(pktLine([]byte("packfile\n")))
		sidebandPack(&buf, pack)
		buf.Write(pktFlush)
	} else {
		// Protocol v0: plain NAK/ACK lines, shallow lines, then the
		// sideband pack and a flush.
		acks := h.ackLines(repo, fr)
		for _, a := range acks {
			buf.Write(pktLine([]byte(a + "\n")))
		}
		for _, s := range plan.Shallow {
			buf.Write(pktLine([]byte("shallow " + s + "\n")))
		}
		for _, s := range plan.Unshallow {
			buf.Write(pktLine([]byte("unshallow " + s + "\n")))
		}
		sidebandPack(&buf, pack)
		buf.Write(pktFlush)
	}
	w.Write(buf.Bytes())
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// hexEncode is a tiny helper kept for debugging pack ids.
var _ = hex.EncodeToString

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	old string
	neu string
	ref string
}

// receivePack handles a protocol v0 receive-pack (push) request. The pack
// is parsed and validated in full before any ref moves, so a corrupt pack
// changes nothing.
func (h *gitHandlers) receivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	// Commands are pkt-lines up to the first flush; the pack follows raw.
	var cmds []pushCommand
	packStart := 0
	{
		rr := bytes.NewReader(body)
		for {
			payload, isFlush, _, err := readPktLine(rr)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "malformed receive-pack request")
				return
			}
			if isFlush {
				packStart = len(body) - rr.Len()
				break
			}
			line := strings.TrimSuffix(string(payload), "\n")
			if i := strings.Index(line, "\x00"); i >= 0 {
				line = line[:i]
			}
			// A client pushing from a shallow clone first declares
			// its shallow boundaries ("shallow <oid>"); these are
			// informational, not ref commands.
			if strings.HasPrefix(line, "shallow ") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 3 || !isOID(fields[0]) || !isOID(fields[1]) || !strings.HasPrefix(fields[2], "refs/") {
				writeErr(w, http.StatusBadRequest, "malformed receive-pack command")
				return
			}
			cmds = append(cmds, pushCommand{old: fields[0], neu: fields[1], ref: fields[2]})
		}
	}
	if len(cmds) == 0 {
		writeErr(w, http.StatusBadRequest, "receive-pack request has no commands")
		return
	}

	pack := body[packStart:]
	needsPack := false
	for _, c := range cmds {
		if c.neu != ZeroOID {
			needsPack = true
		}
	}

	var unpackErr error
	var entries []*PackEntry
	if needsPack {
		if len(pack) == 0 {
			unpackErr = fmt.Errorf("pack is missing")
		} else {
			h.store.mu.RLock()
			entries, unpackErr = ReadPack(pack, h.store.Lookup)
			h.store.mu.RUnlock()
		}
	}

	// Report lines: one "unpack ..." line, then one line per ref.
	type reportLine struct {
		ok     bool
		ref    string
		reason string
	}
	var reports []reportLine
	if unpackErr != nil {
		for _, c := range cmds {
			reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "unpack failed"})
		}
	} else {
		// Store the new objects first; refs move only after.
		// ReadPack already resolved each entry's id, so store by it
		// instead of re-hashing every object.
		h.store.mu.Lock()
		for _, e := range entries {
			h.store.PutObjectWithOID(&Obj{Type: e.Type, Data: e.Data}, e.OID)
		}
		for _, c := range cmds {
			current := repo.Ref(c.ref)
			switch {
			case c.old == ZeroOID && c.neu == ZeroOID:
				reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "invalid old/new"})
			case c.neu == ZeroOID: // delete
				if current != c.old {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "old value does not match"})
				} else {
					repo.DelRef(c.ref)
					reports = append(reports, reportLine{ok: true, ref: c.ref})
				}
			case c.old == ZeroOID: // create
				if current != "" {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "reference already exists"})
				} else if !h.newObjectOK(c) {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "new object is not a commit"})
				} else {
					repo.SetRef(c.ref, c.neu)
					reports = append(reports, reportLine{ok: true, ref: c.ref})
				}
			default: // update
				if current != c.old {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "old value does not match"})
				} else if !h.newObjectOK(c) {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "new object is not a commit"})
				} else if !IsAncestor(h.store.Lookup, c.old, c.neu) {
					reports = append(reports, reportLine{ok: false, ref: c.ref, reason: "non-fast-forward"})
				} else {
					repo.SetRef(c.ref, c.neu)
					reports = append(reports, reportLine{ok: true, ref: c.ref})
				}
			}
		}
		h.store.mu.Unlock()
	}

	// Response: the report is a pkt-line stream ("unpack ok" or
	// "unpack <detail>", then one "ok"/"ng" line per ref, terminated
	// by an inner flush) carried inside sideband band-1 packets, and
	// the whole sideband stream ends with an outer flush. The inner
	// flush is what the client's packet reader waits for: without it
	// the client reads EOF instead of a flush and reports that the
	// remote hung up, even though every report line parsed fine.
	// (Verified byte-for-byte against reference `git receive-pack`.)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(http.StatusOK)
	var inner bytes.Buffer
	if unpackErr != nil {
		inner.Write(pktLine([]byte("unpack " + unpackErr.Error() + "\n")))
	} else {
		inner.Write(pktLine([]byte("unpack ok\n")))
	}
	for _, rep := range reports {
		if rep.ok {
			inner.Write(pktLine([]byte("ok " + rep.ref + "\n")))
		} else {
			inner.Write(pktLine([]byte("ng " + rep.ref + " " + rep.reason + "\n")))
		}
	}
	inner.Write(pktFlush)
	var buf bytes.Buffer
	sidebandStream(&buf, inner.Bytes())
	buf.Write(pktFlush)
	w.Write(buf.Bytes())
}

// sidebandStream writes data as sideband band-1 pkt-lines, chunked so
// no pkt-line exceeds the 65520-byte limit. The data itself is a byte
// stream (a pack, or an inner pkt-line stream for the push report).
func sidebandStream(w io.Writer, data []byte) {
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		outer := make([]byte, 1, 1+n)
		outer[0] = 1
		outer = append(outer, data[:n]...)
		w.Write(pktLine(outer))
		data = data[n:]
	}
}

// newObjectOK checks that a push's new tip exists and has the right kind
// for its ref: a commit for a branch, any object for a tag.
func (h *gitHandlers) newObjectOK(c pushCommand) bool {
	obj, ok := h.store.Lookup(c.neu)
	if !ok {
		return false
	}
	if strings.HasPrefix(c.ref, "refs/heads/") {
		return obj.Type == "commit"
	}
	return true
}

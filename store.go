// In-memory store: the single source of truth for every surface (REST,
// GraphQL, MCP, git). All state lives here, in one process, behind one
// read/write lock. Identifiers are allocated from counters in creation
// order, so a replay of the same requests yields the same ids. Times come
// only from the trial-provided now value, never the wall clock.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Viewer is the fixed identity the trial's bearer token names: the
// scenario's viewer, derived from the contest's fixed seed.
const (
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	viewerID    = 1000
)

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID     int64
	Body   string
	Author string
}

type Issue struct {
	ID         int64
	Number     int
	Title      string
	Body       string
	State      string // "open" or "closed"
	Labels     []string
	Comments   []*Comment
	CreatedAt  string
	UpdatedAt  string
	cachedJSON []byte
}

type PRFile struct {
	Filename  string
	Status    string // "added", "modified", "removed"
	Additions int
	Deletions int
	Changes   int
}

type PR struct {
	ID         int64
	Number     int
	Title      string
	Body       string
	Head       string // branch name
	Base       string // branch name
	State      string // "open" or "closed"
	Merged     bool
	MergedAt   string
	MergeSHA   string
	Files      []PRFile
	CreatedAt  string
	UpdatedAt  string
	cachedJSON []byte
}

type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	Refs       map[string]string // full ref name -> tip oid
	RefVersion int64             // bumped on every ref change

	Labels      map[string]*Label
	Issues      []*Issue
	issuesByNum map[int]*Issue
	nextIssue   int
	PRs         []*PR
	prsByNum    map[int]*PR
	nextPR      int

	cachedJSON []byte
	// Advertisement cache, keyed by RefVersion.
	adVersion int64
	adV2      []byte
	adV0Up    []byte
	adV0Recv  []byte
	lsVersion int64
	lsRefs    []byte
}

func (r *Repo) key() string { return r.Owner + "/" + r.Name }

// Store is the whole forge state.
type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo
	repoOrder []*Repo
	objects   map[string]*Obj

	nextRepoID    int64
	nextIssueID   int64
	nextCommentID int64
	nextPRID      int64

	trialUnix int64
	trialISO  string

	packMu    sync.Mutex
	packCache map[string]packCacheEntry
	packOrder []string
}

// packCacheEntry is one cached fetch result: the pack bytes plus the
// shallow-info lines the response must carry with them, so a cache hit
// needs no re-plan at all.
type packCacheEntry struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

func NewStore(trialUnix int64) *Store {
	t := time.Unix(trialUnix, 0).UTC()
	return &Store{
		repos:         make(map[string]*Repo),
		objects:       make(map[string]*Obj),
		nextRepoID:    1,
		nextIssueID:   1,
		nextCommentID: 1,
		nextPRID:      1,
		trialUnix:     trialUnix,
		trialISO:      t.Format(time.RFC3339),
		packCache:     make(map[string]packCacheEntry),
	}
}

// Lookup implements ObjLookup against the object table. Callers must hold
// at least a read lock.
func (s *Store) Lookup(oid string) (*Obj, bool) {
	o, ok := s.objects[oid]
	return o, ok
}

// PutObject stores an object, returning its id. Callers must hold the lock.
func (s *Store) PutObject(o *Obj) string {
	oid := HashObject(o.Type, o.Data)
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = o
	}
	return oid
}

// PutObjectWithOID stores an object whose id the caller already computed
// (ReadPack resolves it while parsing), skipping a second hash. Callers
// must hold the lock.
func (s *Store) PutObjectWithOID(o *Obj, oid string) {
	if _, ok := s.objects[oid]; !ok {
		s.objects[oid] = o
	}
}

// GetRepo returns a repository by owner and name. Callers must hold a lock.
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// CreateRepo creates a repository owned by the viewer. Callers must hold
// the write lock. Returns nil when the name already exists for the owner.
func (s *Store) CreateRepo(owner, name, description string, private bool) *Repo {
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil
	}
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedAt:     s.trialISO,
		UpdatedAt:     s.trialISO,
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		issuesByNum:   make(map[int]*Issue),
		nextIssue:     1,
		prsByNum:      make(map[int]*PR),
		nextPR:        1,
	}
	s.nextRepoID++
	s.repos[key] = r
	s.repoOrder = append(s.repoOrder, r)
	r.rebuildRepoJSON(s)
	return r
}

// DeleteRepo removes a repository and all its state. Callers hold the lock.
func (s *Store) DeleteRepo(owner, name string) bool {
	key := owner + "/" + name
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	for i, other := range s.repoOrder {
		if other == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// Ref returns a ref's tip, or "" when absent. Callers hold a lock.
func (r *Repo) Ref(name string) string { return r.Refs[name] }

// SetRef moves a ref and bumps the version (invalidating ad caches).
// Callers hold the write lock.
func (r *Repo) SetRef(name, oid string) {
	r.Refs[name] = oid
	r.RefVersion++
}

// DelRef removes a ref and bumps the version. Callers hold the write lock.
func (r *Repo) DelRef(name string) {
	delete(r.Refs, name)
	r.RefVersion++
}

// RefNames returns the ref names in sorted order. Callers hold a lock.
func (r *Repo) RefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IdentityLine is the author/committer line for server-built commits.
func (s *Store) IdentityLine() string {
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, s.trialUnix)
}

// ---------------------------------------------------------------- JSON views

type repoJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Owner    struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"owner"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	URL           string `json:"url"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	HasIssues     bool   `json:"has_issues"`
}

func (r *Repo) rebuildRepoJSON(s *Store) {
	var v repoJSON
	v.ID = r.ID
	v.Name = r.Name
	v.FullName = r.Owner + "/" + r.Name
	v.Owner.Login = r.Owner
	v.Owner.ID = viewerID
	v.Owner.Type = "User"
	v.Private = r.Private
	v.Description = r.Description
	v.DefaultBranch = r.DefaultBranch
	v.HTMLURL = "https://example.invalid/" + r.Owner + "/" + r.Name
	v.URL = "/api/v3/repos/" + r.Owner + "/" + r.Name
	v.CreatedAt = r.CreatedAt
	v.UpdatedAt = r.UpdatedAt
	v.HasIssues = true
	b, _ := json.Marshal(v)
	r.cachedJSON = b
}

type issueJSON struct {
	ID        int64       `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	HTMLURL   string      `json:"html_url"`
	URL       string      `json:"url"`
	Comments  int         `json:"comments"`
}

type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

func viewerUserJSON() userJSON {
	return userJSON{Login: viewerLogin, ID: viewerID, Type: "User"}
}

func (r *Repo) issueView(is *Issue) issueJSON {
	var v issueJSON
	v.ID = is.ID
	v.Number = is.Number
	v.Title = is.Title
	v.Body = is.Body
	v.State = is.State
	v.Labels = make([]labelJSON, 0, len(is.Labels))
	for _, name := range is.Labels {
		color := "ededed"
		if l, ok := r.Labels[name]; ok {
			color = l.Color
		}
		v.Labels = append(v.Labels, labelJSON{Name: name, Color: color})
	}
	v.User = viewerUserJSON()
	v.CreatedAt = is.CreatedAt
	v.UpdatedAt = is.UpdatedAt
	v.HTMLURL = "https://example.invalid/" + r.Owner + "/" + r.Name + "/issues/" + itoa(is.Number)
	v.URL = "/api/v3/repos/" + r.Owner + "/" + r.Name + "/issues/" + itoa(is.Number)
	v.Comments = len(is.Comments)
	return v
}

// rebuildIssueJSON re-renders an issue's cached response bytes. Callers
// hold the write lock.
func (r *Repo) rebuildIssueJSON(is *Issue) {
	b, _ := json.Marshal(r.issueView(is))
	is.cachedJSON = b
}

// CreateIssue allocates the next issue number in this repository and
// stores the issue. Callers hold the write lock.
func (s *Store) CreateIssue(r *Repo, title, body string, labels []string) *Issue {
	is := &Issue{
		ID:        s.nextIssueID,
		Number:    r.nextIssue,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string(nil), labels...),
		CreatedAt: s.trialISO,
		UpdatedAt: s.trialISO,
	}
	s.nextIssueID++
	r.nextIssue++
	r.Issues = append(r.Issues, is)
	r.issuesByNum[is.Number] = is
	r.rebuildIssueJSON(is)
	return is
}

// GetIssue returns an issue by number. Callers hold a lock.
func (r *Repo) GetIssue(number int) *Issue { return r.issuesByNum[number] }

// CreateComment appends a comment to an issue. Callers hold the write lock.
func (s *Store) CreateComment(r *Repo, is *Issue, body string) *Comment {
	c := &Comment{ID: s.nextCommentID, Body: body, Author: viewerLogin}
	s.nextCommentID++
	is.Comments = append(is.Comments, c)
	is.UpdatedAt = s.trialISO
	r.rebuildIssueJSON(is)
	return c
}

type prJSON struct {
	ID             int64     `json:"id"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	State          string    `json:"state"`
	Head           prRefJSON `json:"head"`
	Base           prRefJSON `json:"base"`
	Merged         bool      `json:"merged"`
	MergedAt       string    `json:"merged_at,omitempty"`
	MergeCommitSHA string    `json:"merge_commit_sha,omitempty"`
	User           userJSON  `json:"user"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
	HTMLURL        string    `json:"html_url"`
	URL            string    `json:"url"`
}

type prRefJSON struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// PRHeadSHA / PRBaseSHA read the live branch tips for a pull request.
func (s *Store) PRHeadSHA(r *Repo, pr *PR) string { return r.Ref("refs/heads/" + pr.Head) }
func (s *Store) PRBaseSHA(r *Repo, pr *PR) string { return r.Ref("refs/heads/" + pr.Base) }

func (r *Repo) prView(s *Store, pr *PR) prJSON {
	var v prJSON
	v.ID = pr.ID
	v.Number = pr.Number
	v.Title = pr.Title
	v.Body = pr.Body
	v.State = pr.State
	v.Head = prRefJSON{Ref: pr.Head, SHA: s.PRHeadSHA(r, pr)}
	v.Base = prRefJSON{Ref: pr.Base, SHA: s.PRBaseSHA(r, pr)}
	v.Merged = pr.Merged
	v.MergedAt = pr.MergedAt
	v.MergeCommitSHA = pr.MergeSHA
	v.User = viewerUserJSON()
	v.CreatedAt = pr.CreatedAt
	v.UpdatedAt = pr.UpdatedAt
	v.HTMLURL = "https://example.invalid/" + r.Owner + "/" + r.Name + "/pull/" + itoa(pr.Number)
	v.URL = "/api/v3/repos/" + r.Owner + "/" + r.Name + "/pulls/" + itoa(pr.Number)
	return v
}

// rebuildPRJSON re-renders a pull request's cached bytes. Callers hold
// the write lock.
func (r *Repo) rebuildPRJSON(s *Store, pr *PR) {
	b, _ := json.Marshal(r.prView(s, pr))
	pr.cachedJSON = b
}

// CreatePR allocates the next pull-request number and stores it. Callers
// hold the write lock. Files are the diff of base tip -> head tip.
func (s *Store) CreatePR(r *Repo, title, body, head, base string) *PR {
	pr := &PR{
		ID:        s.nextPRID,
		Number:    r.nextPR,
		Title:     title,
		Body:      body,
		Head:      head,
		Base:      base,
		State:     "open",
		CreatedAt: s.trialISO,
		UpdatedAt: s.trialISO,
	}
	s.nextPRID++
	r.nextPR++
	r.PRs = append(r.PRs, pr)
	r.prsByNum[pr.Number] = pr
	pr.Files = s.DiffFiles(r, base, head)
	r.rebuildPRJSON(s, pr)
	return pr
}

// GetPR returns a pull request by number. Callers hold a lock.
func (r *Repo) GetPR(number int) *PR { return r.prsByNum[number] }

// OpenPRForBranch returns the open pull request from a head branch, if any.
func (r *Repo) OpenPRForBranch(head, base string) *PR {
	for _, pr := range r.PRs {
		if pr.State == "open" && pr.Head == head && pr.Base == base {
			return pr
		}
	}
	return nil
}

// DiffFiles computes the file-level diff between the trees of two branch
// tips (base -> head), with line counts. A missing tip means an empty tree.
func (s *Store) DiffFiles(r *Repo, baseBranch, headBranch string) []PRFile {
	baseFiles := map[string]string{}
	if tip := r.Ref("refs/heads/" + baseBranch); tip != "" {
		if f, err := FlatFiles(s.Lookup, tip); err == nil {
			baseFiles = f
		}
	}
	headFiles := map[string]string{}
	if tip := r.Ref("refs/heads/" + headBranch); tip != "" {
		if f, err := FlatFiles(s.Lookup, tip); err == nil {
			headFiles = f
		}
	}
	names := make(map[string]bool)
	for n := range baseFiles {
		names[n] = true
	}
	for n := range headFiles {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var out []PRFile
	for _, name := range sorted {
		bOID, inBase := baseFiles[name]
		hOID, inHead := headFiles[name]
		switch {
		case inHead && !inBase:
			out = append(out, PRFile{Filename: name, Status: "added", Additions: lineCount(s, hOID), Deletions: 0, Changes: lineCount(s, hOID)})
		case !inHead && inBase:
			out = append(out, PRFile{Filename: name, Status: "removed", Additions: 0, Deletions: lineCount(s, bOID), Changes: lineCount(s, bOID)})
		case inHead && inBase && bOID != hOID:
			a, d := lineCount(s, hOID), lineCount(s, bOID)
			out = append(out, PRFile{Filename: name, Status: "modified", Additions: a, Deletions: d, Changes: a + d})
		}
	}
	return out
}

// lineCount counts newline-terminated lines in a blob (at least 1 for a
// non-empty blob without a trailing newline).
func lineCount(s *Store, oid string) int {
	o, ok := s.objects[oid]
	if !ok || o.Type != "blob" {
		return 0
	}
	n := 0
	for _, b := range o.Data {
		if b == '\n' {
			n++
		}
	}
	if len(o.Data) > 0 && o.Data[len(o.Data)-1] != '\n' {
		n++
	}
	return n
}

// PRMergeable reports whether a pull request can merge cleanly: no file
// changed on the head since the merge-base is also changed on the base.
func (s *Store) PRMergeable(r *Repo, pr *PR) bool {
	headTip := r.Ref("refs/heads/" + pr.Head)
	baseTip := r.Ref("refs/heads/" + pr.Base)
	if headTip == "" || baseTip == "" {
		return false
	}
	mb := MergeBase(s.Lookup, headTip, baseTip)
	if mb == "" {
		// No common ancestor: merging is a fast-forward only when the
		// base has no commits of its own; treat as mergeable only if the
		// base tip is empty (it is not, here) — so conflicting.
		return false
	}
	baseFiles, _ := FlatFiles(s.Lookup, mb)
	headFiles, _ := FlatFiles(s.Lookup, headTip)
	curFiles, _ := FlatFiles(s.Lookup, baseTip)
	for path, hOID := range headFiles {
		mbOID, inMB := baseFiles[path]
		if inMB && mbOID == hOID {
			continue
		}
		// The head changed this path since the merge-base.
		curOID, inCur := curFiles[path]
		if inMB {
			// Modified on the head: clean only when the base left
			// the merge-base version alone (deleted or changed
			// on the base is a conflict).
			if !inCur || curOID != mbOID {
				return false
			}
		} else {
			// Added on the head: clean unless the base added the
			// same path with different content.
			if inCur && curOID != hOID {
				return false
			}
		}
	}
	// A file deleted on the head but changed on the base also conflicts.
	for path := range baseFiles {
		if _, inHead := headFiles[path]; !inHead {
			curOID, inCur := curFiles[path]
			mbOID := baseFiles[path]
			if inCur && curOID != mbOID {
				return false
			}
		}
	}
	return true
}

// MergePR performs a squash merge of a pull request onto its base branch.
// It returns the merge commit id, or "" when the merge is conflicted.
// Callers hold the write lock.
func (s *Store) MergePR(r *Repo, pr *PR) string {
	headTip := r.Ref("refs/heads/" + pr.Head)
	baseTip := r.Ref("refs/heads/" + pr.Base)
	if headTip == "" || baseTip == "" {
		return ""
	}
	if !s.PRMergeable(r, pr) {
		return ""
	}
	mb := MergeBase(s.Lookup, headTip, baseTip)
	baseFiles, _ := FlatFiles(s.Lookup, mb)
	headFiles, _ := FlatFiles(s.Lookup, headTip)
	curFiles, _ := FlatFiles(s.Lookup, baseTip)
	merged := make(map[string]string, len(curFiles))
	for k, v := range curFiles {
		merged[k] = v
	}
	for path, hOID := range headFiles {
		mbOID, inMB := baseFiles[path]
		if !inMB || mbOID != hOID {
			merged[path] = hOID
		}
	}
	for path := range baseFiles {
		if _, inHead := headFiles[path]; !inHead {
			delete(merged, path)
		}
	}
	treeOID, err := BuildTreeFromFiles(merged, s.PutObject)
	if err != nil {
		return ""
	}
	msg := "Merge pull request #" + itoa(pr.Number) + " from " + pr.Head + "\n"
	data, err := BuildCommit(treeOID, []string{baseTip}, s.IdentityLine(), s.IdentityLine(), msg)
	if err != nil {
		return ""
	}
	mergeOID := s.PutObject(&Obj{Type: "commit", Data: data})
	r.SetRef("refs/heads/"+pr.Base, mergeOID)
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.trialISO
	pr.MergeSHA = mergeOID
	pr.UpdatedAt = s.trialISO
	r.rebuildPRJSON(s, pr)
	return mergeOID
}

// CommitOnBranch creates a commit on a branch from a flat file map
// (the branch's current files with one file set or removed), updating the
// ref. It returns the new commit id. Callers hold the write lock.
func (s *Store) CommitOnBranch(r *Repo, branch, message string, setFile *string, setOID string, delFile string) (string, error) {
	tip := r.Ref("refs/heads/" + branch)
	files := map[string]string{}
	var parents []string
	if tip != "" {
		f, err := FlatFiles(s.Lookup, tip)
		if err != nil {
			return "", err
		}
		files = f
		parents = []string{tip}
	}
	if setFile != nil {
		files[*setFile] = setOID
	}
	if delFile != "" {
		delete(files, delFile)
	}
	treeOID, err := BuildTreeFromFiles(files, s.PutObject)
	if err != nil {
		return "", err
	}
	msg := message
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	data, err := BuildCommit(treeOID, parents, s.IdentityLine(), s.IdentityLine(), msg)
	if err != nil {
		return "", err
	}
	oid := s.PutObject(&Obj{Type: "commit", Data: data})
	r.SetRef("refs/heads/"+branch, oid)
	return oid, nil
}

// PackCacheGet returns a cached fetch result for key, if present.
func (s *Store) PackCacheGet(key string) (packCacheEntry, bool) {
	s.packMu.Lock()
	defer s.packMu.Unlock()
	b, ok := s.packCache[key]
	return b, ok
}

// PackCachePut stores a fetch result, evicting the oldest entries past
// the cap.
func (s *Store) PackCachePut(key string, entry packCacheEntry) {
	s.packMu.Lock()
	defer s.packMu.Unlock()
	if _, ok := s.packCache[key]; !ok {
		s.packOrder = append(s.packOrder, key)
	}
	s.packCache[key] = entry
	for len(s.packOrder) > 16 {
		old := s.packOrder[0]
		s.packOrder = s.packOrder[1:]
		delete(s.packCache, old)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------- global ids

// GlobalID encodes an object as an opaque GraphQL global id.
func GlobalID(kind, ref string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + ref))
}

// DecodeGlobalID splits a global id into its kind and reference.
func DecodeGlobalID(id string) (kind, ref string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

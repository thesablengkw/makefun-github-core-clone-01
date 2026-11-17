// REST v3 handlers. Every reply is JSON with a non-empty
// X-GitHub-Request-Id (set by the top-level handler); errors are
// {"message": ...}; unknown routes are JSON 404, never HTML.
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type restHandlers struct{ store *Store }

func newRestHandlers(s *Store) *restHandlers { return &restHandlers{store: s} }

func (h *restHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	path = strings.TrimSuffix(path, "/")
	parts := splitPathSegments(path)

	switch {
	case len(parts) == 0 && r.Method == http.MethodGet:
		// GET /api/v3/
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, http.StatusOK, map[string]string{
			"current_user_url": "/api/v3/user",
			"repository_url":   "/api/v3/repos/{owner}/{repo}",
		})
	case len(parts) == 1 && parts[0] == "meta" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"installed_version": "1.0.0"})
	case len(parts) == 1 && parts[0] == "user" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"login": viewerLogin, "id": viewerID, "name": viewerName,
			"email": viewerEmail, "type": "User",
		})
	case len(parts) == 2 && parts[0] == "user" && parts[1] == "repos":
		if r.Method == http.MethodGet {
			h.listUserRepos(w, r)
		} else if r.Method == http.MethodPost {
			h.createUserRepo(w, r)
		} else {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
	case len(parts) >= 2 && parts[0] == "repos":
		h.dispatchRepo(w, r, parts[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func splitPathSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ---------------------------------------------------------------- repos

func (h *restHandlers) listUserRepos(w http.ResponseWriter, r *http.Request) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	page := intQuery(r, "page", 1)
	if page < 1 {
		page = 1
	}
	perPage := intQuery(r, "per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	total := len(h.store.repoOrder)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	if end < total {
		setLinkNext(w, r, page, perPage)
	}
	items := make([]json.RawMessage, 0, end-start)
	for _, repo := range h.store.repoOrder[start:end] {
		items = append(items, repo.cachedJSON)
	}
	writeRawJSON(w, http.StatusOK, marshalJSONArray(items))
}

func marshalJSONArray(items []json.RawMessage) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (h *restHandlers) createUserRepo(w http.ResponseWriter, r *http.Request) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	name := strField(body, "name")
	if name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	repo := h.store.CreateRepo(viewerLogin, name, strField(body, "description"), boolField(body, "private"))
	if repo == nil {
		writeErr(w, http.StatusUnprocessableEntity, "Repository already exists")
		return
	}
	writeRawJSON(w, http.StatusCreated, repo.cachedJSON)
}

// dispatchRepo handles /repos/{owner}/{repo}/... routes.
func (h *restHandlers) dispatchRepo(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := parts[0], parts[1]
	rest := parts[2:]

	// Repository itself.
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.store.mu.RLock()
			repo := h.store.GetRepo(owner, name)
			h.store.mu.RUnlock()
			if repo == nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeRawJSON(w, http.StatusOK, repo.cachedJSON)
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			h.store.mu.Lock()
			repo := h.store.GetRepo(owner, name)
			if repo == nil {
				h.store.mu.Unlock()
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if d, exists := body["description"]; exists {
				if s, isStr := d.(string); isStr {
					repo.Description = s
				}
			}
			repo.rebuildRepoJSON(h.store)
			h.store.mu.Unlock()
			writeRawJSON(w, http.StatusOK, repo.cachedJSON)
		case http.MethodDelete:
			h.store.mu.Lock()
			found := h.store.DeleteRepo(owner, name)
			h.store.mu.Unlock()
			if !found {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}

	switch rest[0] {
	case "branches":
		h.handleBranches(w, r, owner, name, rest[1:])
	case "commits":
		h.handleCommits(w, r, owner, name)
	case "git":
		h.handleGitRefs(w, r, owner, name, rest[1:])
	case "contents":
		h.handleContents(w, r, owner, name, rest[1:])
	case "labels":
		h.handleLabels(w, r, owner, name)
	case "issues":
		h.handleIssues(w, r, owner, name, rest[1:])
	case "pulls":
		h.handlePulls(w, r, owner, name, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// loadRepo fetches a repo or writes 404. Returns nil after writing.
func (h *restHandlers) loadRepo(w http.ResponseWriter, owner, name string) *Repo {
	h.store.mu.RLock()
	repo := h.store.GetRepo(owner, name)
	h.store.mu.RUnlock()
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

// ---------------------------------------------------------------- branches & commits

func (h *restHandlers) handleBranches(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		type branchJSON struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
				URL string `json:"url"`
			} `json:"commit"`
			Protected bool `json:"protected"`
		}
		var out []branchJSON
		for _, refName := range repo.RefNames() {
			if !strings.HasPrefix(refName, "refs/heads/") {
				continue
			}
			branch := strings.TrimPrefix(refName, "refs/heads/")
			var b branchJSON
			b.Name = branch
			b.Commit.SHA = repo.Ref(refName)
			b.Commit.URL = "/api/v3/repos/" + owner + "/" + name + "/commits/" + b.Commit.SHA
			b.Protected = false
			out = append(out, b)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		branch := rest[0]
		sha := repo.Ref("refs/heads/" + branch)
		if sha == "" {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name": branch,
			"commit": map[string]string{
				"sha": sha,
				"url": "/api/v3/repos/" + owner + "/" + name + "/commits/" + sha,
			},
			"protected": false,
		})
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (h *restHandlers) handleCommits(w http.ResponseWriter, r *http.Request, owner, name string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	ref := r.URL.Query().Get("sha")
	tip := ""
	if ref == "" {
		tip = repo.Ref("refs/heads/" + repo.DefaultBranch)
	} else if strings.HasPrefix(ref, "refs/") {
		tip = repo.Ref(ref)
	} else if isOID(ref) {
		tip = ref
	} else {
		tip = repo.Ref("refs/heads/" + ref)
	}
	type commitJSON struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"committer"`
		} `json:"commit"`
	}
	var out []commitJSON
	// First-parent walk from the tip.
	seen := make(map[string]bool)
	for cur := tip; cur != "" && !seen[cur]; {
		seen[cur] = true
		obj, ok := h.store.Lookup(cur)
		if !ok || obj.Type != "commit" {
			break
		}
		cv, err := ParseCommit(obj.Data)
		if err != nil {
			break
		}
		var c commitJSON
		c.SHA = cur
		c.Commit.Message = strings.TrimSuffix(cv.Message, "\n")
		c.Commit.Author.Name = viewerName
		c.Commit.Author.Email = viewerEmail
		c.Commit.Committer.Name = viewerName
		c.Commit.Committer.Email = viewerEmail
		out = append(out, c)
		if len(cv.Parents) == 0 {
			break
		}
		cur = cv.Parents[0]
	}
	if out == nil {
		out = []commitJSON{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- git refs

func (h *restHandlers) handleGitRefs(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	if len(rest) == 1 && rest[0] == "refs" && r.Method == http.MethodPost {
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		ref := strField(body, "ref")
		sha := strField(body, "sha")
		if ref == "" || !isOID(sha) {
			writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
			return
		}
		h.store.mu.Lock()
		if repo.Ref(ref) != "" {
			h.store.mu.Unlock()
			writeErr(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		if _, exists := h.store.Lookup(sha); !exists {
			h.store.mu.Unlock()
			writeErr(w, http.StatusUnprocessableEntity, "Object does not exist")
			return
		}
		repo.SetRef(ref, sha)
		h.store.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{
			"ref":    ref,
			"object": map[string]string{"sha": sha, "type": "commit"},
		})
		return
	}
	if len(rest) >= 2 && rest[0] == "refs" && r.Method == http.MethodGet {
		// /git/refs/heads/{branch}
		refName := strings.Join(rest[1:], "/")
		h.store.mu.RLock()
		sha := repo.Ref(refName)
		h.store.mu.RUnlock()
		if sha == "" {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ref": refName,
			"object": map[string]string{
				"sha":  sha,
				"type": "commit",
				"url":  "/api/v3/repos/" + owner + "/" + name + "/git/commits/" + sha,
			},
		})
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------- contents

// resolveContentRef returns the commit tip for a ref query (?ref=branch,
// ?ref=sha, or the default branch).
func (h *restHandlers) resolveContentRef(repo *Repo, r *http.Request) string {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		return repo.Ref("refs/heads/" + repo.DefaultBranch)
	}
	if isOID(ref) {
		return ref
	}
	if strings.HasPrefix(ref, "refs/") {
		return repo.Ref(ref)
	}
	return repo.Ref("refs/heads/" + ref)
}

func (h *restHandlers) handleContents(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	path := strings.Join(rest, "/")
	switch r.Method {
	case http.MethodGet:
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		tip := h.resolveContentRef(repo, r)
		if tip == "" {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		files, err := FlatFiles(h.store.Lookup, tip)
		if err != nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if path == "" {
			h.writeDirListing(w, repo, files, "")
			return
		}
		if oid, ok := files[path]; ok {
			obj, exists := h.store.Lookup(oid)
			if !exists {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"type": "file", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(obj.Data),
				"sha":     oid, "size": len(obj.Data),
				"name": path[strings.LastIndex(path, "/")+1:], "path": path,
			})
			return
		}
		// Directory listing: any file under path/.
		prefix := path + "/"
		children := map[string]bool{}
		isDir := false
		for p := range files {
			if strings.HasPrefix(p, prefix) {
				isDir = true
				rest := p[len(prefix):]
				seg := rest
				if i := strings.Index(rest, "/"); i >= 0 {
					seg = rest[:i]
				}
				children[seg] = true
			}
		}
		if !isDir {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		var out []map[string]any
		names := make([]string, 0, len(children))
		for n := range children {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			full := prefix + n
			typ := "dir"
			sha := ""
			if oid, ok := files[full]; ok {
				typ = "file"
				sha = oid
			}
			out = append(out, map[string]any{
				"type": typ, "name": n, "path": full, "sha": sha,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPut:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		contentB64 := strField(body, "content")
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contentB64, "\n", ""))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "content is not valid base64")
			return
		}
		branch := strField(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := strField(body, "message")
		if message == "" {
			message = "Update " + path
		}
		sha := strField(body, "sha")
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		tip := repo.Ref("refs/heads/" + branch)
		files := map[string]string{}
		if tip != "" {
			f, ferr := FlatFiles(h.store.Lookup, tip)
			if ferr == nil {
				files = f
			}
		}
		_, exists := files[path]
		if exists && sha != "" && sha != files[path] {
			writeErr(w, http.StatusConflict, "sha does not match the current file")
			return
		}
		blobOID := h.store.PutObject(&Obj{Type: "blob", Data: data})
		commitOID, err := h.store.CommitOnBranch(repo, branch, message, &path, blobOID, "")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not create commit")
			return
		}
		status := http.StatusCreated
		if exists {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{
			"content": map[string]any{
				"name": path[strings.LastIndex(path, "/")+1:], "path": path,
				"sha": blobOID, "size": len(data), "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(data),
			},
			"commit": map[string]string{"sha": commitOID, "message": message},
		})
	case http.MethodDelete:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		branch := strField(body, "branch")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := strField(body, "message")
		if message == "" {
			message = "Delete " + path
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		tip := repo.Ref("refs/heads/" + branch)
		if tip == "" {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		files, err := FlatFiles(h.store.Lookup, tip)
		if err != nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if _, exists := files[path]; !exists {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		commitOID, err := h.store.CommitOnBranch(repo, branch, message, nil, "", path)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not create commit")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"commit": map[string]string{"sha": commitOID, "message": message},
		})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (h *restHandlers) writeDirListing(w http.ResponseWriter, repo *Repo, files map[string]string, prefix string) {
	children := map[string]bool{}
	for p := range files {
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		seg := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			seg = rest[:i]
		}
		children[seg] = true
	}
	var out []map[string]any
	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		full := prefix + n
		typ := "dir"
		sha := ""
		if oid, ok := files[full]; ok {
			typ = "file"
			sha = oid
		}
		out = append(out, map[string]any{"type": typ, "name": n, "path": full, "sha": sha})
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- labels

func (h *restHandlers) handleLabels(w http.ResponseWriter, r *http.Request, owner, name string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		var out []labelJSON
		names := make([]string, 0, len(repo.Labels))
		for n := range repo.Labels {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, labelJSON{Name: n, Color: repo.Labels[n].Color})
		}
		if out == nil {
			out = []labelJSON{}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		body, ok := readJSONBody(w, r)
		if !ok {
			return
		}
		labelName := strField(body, "name")
		color := strField(body, "color")
		if labelName == "" {
			writeErr(w, http.StatusUnprocessableEntity, "name is required")
			return
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		if _, exists := repo.Labels[labelName]; exists {
			writeErr(w, http.StatusUnprocessableEntity, "Label already exists")
			return
		}
		repo.Labels[labelName] = &Label{Name: labelName, Color: color}
		writeJSON(w, http.StatusCreated, labelJSON{Name: labelName, Color: color})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

// ---------------------------------------------------------------- issues

func (h *restHandlers) handleIssues(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listIssues(w, r, repo)
		case http.MethodPost:
			h.createIssue(w, r, repo)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil || num < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			h.store.mu.RLock()
			is := repo.GetIssue(num)
			h.store.mu.RUnlock()
			if is == nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			var out []map[string]any
			for _, c := range is.Comments {
				out = append(out, map[string]any{
					"id": c.ID, "body": c.Body,
					"user":       viewerUserJSON(),
					"created_at": h.store.trialISO,
				})
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			h.store.mu.Lock()
			is := repo.GetIssue(num)
			if is == nil {
				h.store.mu.Unlock()
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			c := h.store.CreateComment(repo, is, strField(body, "body"))
			h.store.mu.Unlock()
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": c.ID, "body": c.Body,
				"user":       viewerUserJSON(),
				"created_at": h.store.trialISO,
			})
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.store.mu.RLock()
			is := repo.GetIssue(num)
			h.store.mu.RUnlock()
			if is == nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeRawJSON(w, http.StatusOK, is.cachedJSON)
		case http.MethodPatch:
			body, ok := readJSONBody(w, r)
			if !ok {
				return
			}
			h.store.mu.Lock()
			is := repo.GetIssue(num)
			if is == nil {
				h.store.mu.Unlock()
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if s, exists := body["state"]; exists {
				if st, isStr := s.(string); isStr && (st == "open" || st == "closed") {
					is.State = st
				}
			}
			if s, exists := body["title"]; exists {
				if t, isStr := s.(string); isStr {
					is.Title = t
				}
			}
			if s, exists := body["body"]; exists {
				if b, isStr := s.(string); isStr {
					is.Body = b
				}
			}
			is.UpdatedAt = h.store.trialISO
			repo.rebuildIssueJSON(is)
			h.store.mu.Unlock()
			writeRawJSON(w, http.StatusOK, is.cachedJSON)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (h *restHandlers) listIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	sortBy := r.URL.Query().Get("sort")
	direction := r.URL.Query().Get("direction")
	page := intQuery(r, "page", 1)
	if page < 1 {
		page = 1
	}
	perPage := intQuery(r, "per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	var filtered []*Issue
	for _, is := range repo.Issues {
		if state != "all" && is.State != state {
			continue
		}
		filtered = append(filtered, is)
	}
	if sortBy == "created" && direction == "desc" {
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Number > filtered[j].Number })
	} else if sortBy == "created" && direction == "asc" {
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Number < filtered[j].Number })
	}
	total := len(filtered)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	if end < total {
		setLinkNext(w, r, page, perPage)
	}
	items := make([]json.RawMessage, 0, end-start)
	for _, is := range filtered[start:end] {
		items = append(items, is.cachedJSON)
	}
	writeRawJSON(w, http.StatusOK, marshalJSONArray(items))
}

func (h *restHandlers) createIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	title := strField(body, "title")
	if title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	var labels []string
	if raw, exists := body["labels"]; exists {
		if arr, isArr := raw.([]any); isArr {
			for _, item := range arr {
				switch v := item.(type) {
				case string:
					labels = append(labels, v)
				case map[string]any:
					if n, ok := v["name"].(string); ok {
						labels = append(labels, n)
					}
				}
			}
		}
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for _, name := range labels {
		if _, exists := repo.Labels[name]; !exists {
			writeErr(w, http.StatusUnprocessableEntity, "Label does not exist")
			return
		}
	}
	is := h.store.CreateIssue(repo, title, strField(body, "body"), labels)
	writeRawJSON(w, http.StatusCreated, is.cachedJSON)
}

// ---------------------------------------------------------------- pulls

func (h *restHandlers) handlePulls(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := h.loadRepo(w, owner, name)
	if repo == nil {
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.listPulls(w, r, repo)
		case http.MethodPost:
			h.createPull(w, r, repo)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil || num < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		h.store.mu.RLock()
		pr := repo.GetPR(num)
		h.store.mu.RUnlock()
		if pr == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		type fileJSON struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Changes   int    `json:"changes"`
		}
		var out []fileJSON
		for _, f := range pr.Files {
			out = append(out, fileJSON{f.Filename, f.Status, f.Additions, f.Deletions, f.Changes})
		}
		if out == nil {
			out = []fileJSON{}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		h.store.mu.Lock()
		pr := repo.GetPR(num)
		if pr == nil {
			h.store.mu.Unlock()
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		if pr.State != "open" {
			h.store.mu.Unlock()
			writeErr(w, http.StatusMethodNotAllowed, "Pull request is already closed")
			return
		}
		if !h.store.PRMergeable(repo, pr) {
			h.store.mu.Unlock()
			writeErr(w, http.StatusMethodNotAllowed, "Pull request is conflicted")
			return
		}
		mergeOID := h.store.MergePR(repo, pr)
		h.store.mu.Unlock()
		if mergeOID == "" {
			writeErr(w, http.StatusMethodNotAllowed, "Pull request is conflicted")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sha": mergeOID, "merged": true,
			"message": "Pull request successfully merged",
		})
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		h.store.mu.RLock()
		pr := repo.GetPR(num)
		h.store.mu.RUnlock()
		if pr == nil {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		writeRawJSON(w, http.StatusOK, pr.cachedJSON)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

func (h *restHandlers) listPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	var filtered []*PR
	for _, pr := range repo.PRs {
		if state == "all" || pr.State == state {
			filtered = append(filtered, pr)
		}
	}
	items := make([]json.RawMessage, 0, len(filtered))
	for _, pr := range filtered {
		items = append(items, pr.cachedJSON)
	}
	writeRawJSON(w, http.StatusOK, marshalJSONArray(items))
}

func (h *restHandlers) createPull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return
	}
	title := strField(body, "title")
	head := strField(body, "head")
	base := strField(body, "base")
	if title == "" || head == "" || base == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title, head and base are required")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if repo.Ref("refs/heads/"+head) == "" {
		writeErr(w, http.StatusNotFound, "Head branch does not exist")
		return
	}
	if repo.OpenPRForBranch(head, base) != nil {
		writeErr(w, http.StatusUnprocessableEntity, "A pull request already exists for "+head)
		return
	}
	pr := h.store.CreatePR(repo, title, strField(body, "body"), head, base)
	writeRawJSON(w, http.StatusCreated, pr.cachedJSON)
}

// strconv is used by handleIssues; keep the import referenced.
var _ = strconv.Itoa

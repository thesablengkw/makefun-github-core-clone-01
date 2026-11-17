// GraphQL endpoint: a small hand-written parser and resolver for the
// documents the conformance suite sends. Supports variables, inline enum
// lists, inline input objects, null variables, operation names, aliases
// and inline fragments. Batched requests (a JSON array of documents) get
// an array of responses in order.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type graphqlHandlers struct{ store *Store }

func newGraphQLHandlers(s *Store) *graphqlHandlers { return &graphqlHandlers{store: s} }

// ---------------------------------------------------------------- parser

type gqlValue struct {
	kind string // "var","string","int","bool","null","enum","list","object"
	str  string
	num  int64
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlSelection struct {
	alias    string
	name     string
	args     map[string]*gqlValue
	children []*gqlSelection
	fragment string // inline fragment type condition, "" when not a fragment
}

type gqlOperation struct {
	kind      string // "query" or "mutation"
	name      string
	variables map[string]*gqlValue // variable defaults
	sel       []*gqlSelection
}

type gqlLexer struct {
	src string
	pos int
}

func (l *gqlLexer) skipSpace() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			l.pos++
			continue
		}
		if c == '#' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
}

func (l *gqlLexer) peek() byte {
	l.skipSpace()
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *gqlLexer) next() byte {
	l.skipSpace()
	if l.pos >= len(l.src) {
		return 0
	}
	c := l.src[l.pos]
	l.pos++
	return c
}

func (l *gqlLexer) expect(c byte) error {
	if l.next() != c {
		return fmt.Errorf("expected %q", c)
	}
	return nil
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func (l *gqlLexer) name() (string, error) {
	l.skipSpace()
	start := l.pos
	if l.pos >= len(l.src) || !isNameStart(l.src[l.pos]) {
		return "", fmt.Errorf("expected a name at offset %d", l.pos)
	}
	for l.pos < len(l.src) && isNameChar(l.src[l.pos]) {
		l.pos++
	}
	return l.src[start:l.pos], nil
}

func (l *gqlLexer) str() (string, error) {
	// Assumes the next non-space char is '"'.
	if l.next() != '"' {
		return "", fmt.Errorf("expected a string")
	}
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		l.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if l.pos >= len(l.src) {
				return "", fmt.Errorf("unterminated string escape")
			}
			e := l.src[l.pos]
			l.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u':
				if l.pos+4 > len(l.src) {
					return "", fmt.Errorf("bad unicode escape")
				}
				n, err := strconv.ParseInt(l.src[l.pos:l.pos+4], 16, 32)
				if err != nil {
					return "", err
				}
				l.pos += 4
				b.WriteRune(rune(n))
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (l *gqlLexer) number() (*gqlValue, error) {
	l.skipSpace()
	start := l.pos
	if l.pos < len(l.src) && l.src[l.pos] == '-' {
		l.pos++
	}
	for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
		l.pos++
	}
	n, err := strconv.ParseInt(l.src[start:l.pos], 10, 64)
	if err != nil {
		return nil, err
	}
	return &gqlValue{kind: "int", num: n}, nil
}

// parseValue parses one GraphQL value.
func (l *gqlLexer) parseValue() (*gqlValue, error) {
	switch c := l.peek(); {
	case c == '"':
		s, err := l.str()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "string", str: s}, nil
	case c == '$':
		l.next()
		n, err := l.name()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", str: n}, nil
	case c == '[':
		l.next()
		v := &gqlValue{kind: "list"}
		for l.peek() != ']' {
			if l.peek() == 0 {
				return nil, fmt.Errorf("unterminated list")
			}
			item, err := l.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, item)
		}
		l.next()
		return v, nil
	case c == '{':
		l.next()
		v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for l.peek() != '}' {
			if l.peek() == 0 {
				return nil, fmt.Errorf("unterminated input object")
			}
			key, err := l.name()
			if err != nil {
				return nil, err
			}
			if err := l.expect(':'); err != nil {
				return nil, err
			}
			val, err := l.parseValue()
			if err != nil {
				return nil, err
			}
			v.obj[key] = val
		}
		l.next()
		return v, nil
	case c == '-' || c >= '0' && c <= '9':
		return l.number()
	case isNameStart(c):
		n, err := l.name()
		if err != nil {
			return nil, err
		}
		switch n {
		case "true":
			return &gqlValue{kind: "bool", str: "true"}, nil
		case "false":
			return &gqlValue{kind: "bool", str: "false"}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: n}, nil
		}
	default:
		return nil, fmt.Errorf("unexpected character %q in value", c)
	}
}

// parseSelectionSet parses "{ ... }".
func (l *gqlLexer) parseSelectionSet() ([]*gqlSelection, error) {
	if err := l.expect('{'); err != nil {
		return nil, err
	}
	var out []*gqlSelection
	for {
		if l.peek() == '}' {
			l.next()
			return out, nil
		}
		if l.peek() == 0 {
			return nil, fmt.Errorf("unterminated selection set")
		}
		sel, err := l.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
}

func (l *gqlLexer) parseSelection() (*gqlSelection, error) {
	// Inline fragment: "... on Type { ... }" or "... { ... }".
	if l.peek() == '.' {
		if l.pos+2 >= len(l.src) || l.src[l.pos+1] != '.' || l.src[l.pos+2] != '.' {
			return nil, fmt.Errorf("bad fragment spread")
		}
		l.pos += 3
		sel := &gqlSelection{}
		n, err := l.name()
		if err != nil {
			return nil, err
		}
		if n == "on" {
			cond, err := l.name()
			if err != nil {
				return nil, err
			}
			sel.fragment = cond
		} else {
			// Named fragment spread (not used by the suite): skip its
			// selection set if present.
			sel.fragment = n
			if l.peek() == '{' {
				children, err := l.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				sel.children = children
			}
			return sel, nil
		}
		children, err := l.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.children = children
		return sel, nil
	}
	sel := &gqlSelection{}
	first, err := l.name()
	if err != nil {
		return nil, err
	}
	if l.peek() == ':' {
		l.next()
		sel.alias = first
		name, err := l.name()
		if err != nil {
			return nil, err
		}
		sel.name = name
	} else {
		sel.name = first
	}
	if l.peek() == '(' {
		l.next()
		sel.args = map[string]*gqlValue{}
		for l.peek() != ')' {
			if l.peek() == 0 {
				return nil, fmt.Errorf("unterminated argument list")
			}
			key, err := l.name()
			if err != nil {
				return nil, err
			}
			if err := l.expect(':'); err != nil {
				return nil, err
			}
			val, err := l.parseValue()
			if err != nil {
				return nil, err
			}
			sel.args[key] = val
		}
		l.next()
	}
	// Directives (@...) are not used by the suite; skip them if present.
	for l.peek() == '@' {
		l.next()
		if _, err := l.name(); err != nil {
			return nil, err
		}
		if l.peek() == '(' {
			l.next()
			depth := 1
			for depth > 0 {
				c := l.next()
				if c == 0 {
					return nil, fmt.Errorf("unterminated directive")
				}
				if c == '(' {
					depth++
				} else if c == ')' {
					depth--
				}
			}
		}
	}
	if l.peek() == '{' {
		children, err := l.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.children = children
	}
	return sel, nil
}

// parseOperation parses one operation (query/mutation or shorthand).
func (l *gqlLexer) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", variables: map[string]*gqlValue{}}
	if c := l.peek(); c == '{' {
		sel, err := l.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		op.sel = sel
		return op, nil
	}
	kind, err := l.name()
	if err != nil {
		return nil, err
	}
	if kind != "query" && kind != "mutation" && kind != "subscription" {
		return nil, fmt.Errorf("unknown operation %q", kind)
	}
	op.kind = kind
	if c := l.peek(); isNameStart(c) {
		n, err := l.name()
		if err != nil {
			return nil, err
		}
		op.name = n
	}
	if l.peek() == '(' {
		l.next()
		for l.peek() != ')' {
			if l.peek() == 0 {
				return nil, fmt.Errorf("unterminated variable definitions")
			}
			if l.peek() == '$' {
				l.next()
			}
			vname, err := l.name()
			if err != nil {
				return nil, err
			}
			if err := l.expect(':'); err != nil {
				return nil, err
			}
			// Type: a name, possibly wrapped in [ ] or suffixed with !.
			for {
				c := l.peek()
				if c == '[' {
					l.next()
					continue
				}
				if c == ']' || c == '!' {
					l.next()
					continue
				}
				break
			}
			if _, err := l.name(); err != nil {
				return nil, err
			}
			for {
				c := l.peek()
				if c == ']' || c == '!' {
					l.next()
					continue
				}
				break
			}
			if l.peek() == '=' {
				l.next()
				def, err := l.parseValue()
				if err != nil {
					return nil, err
				}
				op.variables[vname] = def
			}
		}
		l.next()
	}
	sel, err := l.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

// parseDocument parses a whole GraphQL document (one operation).
func parseDocument(src string) (*gqlOperation, error) {
	l := &gqlLexer{src: src}
	op, err := l.parseOperation()
	if err != nil {
		return nil, err
	}
	return op, nil
}

// resolveValue substitutes variables in a parsed value.
func resolveValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		if val, ok := vars[v.str]; ok {
			return val
		}
		return nil
	case "string":
		return v.str
	case "int":
		return v.num
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "enum":
		return v.str
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = resolveValue(item, vars)
		}
		return out
	}
	return nil
}

func argString(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

// ---------------------------------------------------------------- executor

// gqlObj is a resolved object with its GraphQL type name.
type gqlObj struct {
	typ string
	val any
}

type gqlExecutor struct {
	store  *Store
	errors []map[string]any
}

type viewerObj struct{}

type repoObj struct{ repo *Repo }
type issueObj struct {
	repo  *Repo
	issue *Issue
}
type prObj struct {
	repo *Repo
	pr   *PR
}
type userObj struct {
	login string
	id    int64
}
type refObj struct{ name string }
type connObj struct {
	nodes     []*gqlObj
	hasNext   bool
	endCursor string
}
type fileObj struct{ path string }
type typeObj struct{ name string }
type commentObj struct {
	repo    *Repo
	issue   *Issue
	comment *Comment
}

var typeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author", "labels", "comments"},
	"PullRequest": {"id", "number", "title", "state", "headRefName", "baseRefName", "mergeable", "merged", "files", "body", "createdAt", "author"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "createdAt", "updatedAt", "url"},
	"User":        {"id", "login", "name", "email"},
	"Comment":     {"id", "body", "author", "createdAt"},
}

// ServeHTTP handles one GraphQL request (or a batch).
func (h *graphqlHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeErr(w, http.StatusBadRequest, "empty GraphQL request")
		return
	}
	// Batched documents: a JSON array.
	if trimmed[0] == '[' {
		var docs []map[string]any
		if err := json.Unmarshal(trimmed, &docs); err != nil {
			writeErr(w, http.StatusBadRequest, "malformed GraphQL batch")
			return
		}
		out := make([]any, 0, len(docs))
		for _, doc := range docs {
			out = append(out, h.executeDoc(doc))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed GraphQL request")
		return
	}
	writeJSON(w, http.StatusOK, h.executeDoc(doc))
}

// executeDoc runs one document and returns its response object.
func (h *graphqlHandlers) executeDoc(doc map[string]any) map[string]any {
	query, _ := doc["query"].(string)
	vars := map[string]any{}
	if v, ok := doc["variables"].(map[string]any); ok {
		vars = v
	}
	op, err := parseDocument(query)
	if err != nil {
		return map[string]any{"errors": []map[string]any{{"message": err.Error()}}}
	}
	ex := &gqlExecutor{store: h.store}
	// Mutations hold the write lock for the whole operation so a
	// read-after-write inside one document is atomic.
	if op.kind == "mutation" {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
	} else {
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ex.execObject(rootType, nil, op.sel, vars)
	resp := map[string]any{"data": data}
	if len(ex.errors) > 0 {
		resp["errors"] = ex.errors
	}
	return resp
}

// execObject resolves a selection set against one object.
func (ex *gqlExecutor) execObject(typeName string, obj any, sels []*gqlSelection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range sels {
		if sel.fragment != "" {
			// Inline fragment: only applies when the type matches.
			if sel.fragment != typeName {
				continue
			}
			for k, v := range ex.execObject(typeName, obj, sel.children, vars) {
				out[k] = v
			}
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		val, ok := ex.resolveField(typeName, obj, sel, vars)
		if !ok {
			ex.errors = append(ex.errors, map[string]any{
				"message": fmt.Sprintf("Cannot query field %q on type %q.", sel.name, typeName),
			})
			out[key] = nil
			continue
		}
		out[key] = ex.expandValue(val, sel, vars)
	}
	return out
}

// expandValue recurses into object values that have sub-selections.
func (ex *gqlExecutor) expandValue(val any, sel *gqlSelection, vars map[string]any) any {
	if len(sel.children) == 0 {
		return val
	}
	switch v := val.(type) {
	case *gqlObj:
		return ex.execObject(v.typ, v.val, sel.children, vars)
	case []*gqlObj:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, ex.execObject(item.typ, item.val, sel.children, vars))
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, ex.expandValue(item, sel, vars))
		}
		return out
	}
	return val
}

func (ex *gqlExecutor) argsOf(sel *gqlSelection, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range sel.args {
		out[k] = resolveValue(v, vars)
	}
	return out
}

// resolveField resolves one field on one object. The second return value
// is false for an unknown field.
func (ex *gqlExecutor) resolveField(typeName string, obj any, sel *gqlSelection, vars map[string]any) (any, bool) {
	if sel.name == "__typename" {
		return typeName, true
	}
	args := ex.argsOf(sel, vars)
	switch typeName {
	case "Query", "Mutation":
		return ex.resolveRoot(typeName, sel, args)
	case "Repository":
		return ex.resolveRepository(obj.(*repoObj).repo, sel, args)
	case "Issue":
		return ex.resolveIssue(obj.(*issueObj), sel, args)
	case "PullRequest":
		return ex.resolvePR(obj.(*prObj), sel, args)
	case "User":
		u := obj.(*userObj)
		switch sel.name {
		case "login":
			return u.login, true
		case "id":
			return u.id, true
		case "name":
			return u.login, true
		case "email":
			return u.login + "@example.invalid", true
		}
	case "BranchRef":
		if sel.name == "name" {
			return obj.(*refObj).name, true
		}
	case "IssueConnection", "PullRequestConnection", "FileConnection":
		c := obj.(*connObj)
		switch sel.name {
		case "nodes":
			return c.nodes, true
		case "pageInfo":
			return &gqlObj{typ: "PageInfo", val: c}, true
		}
	case "PageInfo":
		c := obj.(*connObj)
		switch sel.name {
		case "hasNextPage":
			return c.hasNext, true
		case "endCursor":
			return c.endCursor, true
		}
	case "Comment":
		c := obj.(*commentObj)
		switch sel.name {
		case "id":
			return GlobalID("comment", c.repo.key()+":"+itoa(c.issue.Number)+":"+itoa(int(c.comment.ID))), true
		case "body":
			return c.comment.Body, true
		case "author":
			return &gqlObj{typ: "User", val: &userObj{login: c.comment.Author, id: viewerID}}, true
		case "createdAt":
			return ex.store.trialISO, true
		}
	case "File":
		if sel.name == "path" {
			return obj.(*fileObj).path, true
		}
	case "__Type":
		t := obj.(*typeObj)
		switch sel.name {
		case "name":
			return t.name, true
		case "fields":
			var out []*gqlObj
			for _, f := range typeFields[t.name] {
				out = append(out, &gqlObj{typ: "__Field", val: f})
			}
			return out, true
		case "enumValues":
			var out []*gqlObj
			for _, e := range enumValuesFor(t.name) {
				out = append(out, &gqlObj{typ: "__EnumValue", val: e})
			}
			return out, true
		}
	case "__Field":
		if sel.name == "name" {
			return obj.(string), true
		}
	case "__EnumValue":
		if sel.name == "name" {
			return obj.(string), true
		}
	case "IssueEdge":
		if sel.name == "node" {
			return obj, true
		}
	case "CommentEdge":
		if sel.name == "node" {
			return obj, true
		}
	case "CreateIssuePayload":
		if sel.name == "issue" {
			return obj, true
		}
	case "CloseIssuePayload":
		if sel.name == "issue" {
			return obj, true
		}
	case "CreatePullRequestPayload":
		if sel.name == "pullRequest" {
			return obj, true
		}
	case "MergePullRequestPayload":
		if sel.name == "pullRequest" {
			return obj, true
		}
	case "AddCommentPayload":
		if sel.name == "commentEdge" {
			return obj, true
		}
	}
	return nil, false
}

func enumValuesFor(typeName string) []string {
	if typeName == "SearchType" {
		return []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"}
	}
	return nil
}

// ---------------------------------------------------------------- root

func (ex *gqlExecutor) resolveRoot(typeName string, sel *gqlSelection, args map[string]any) (any, bool) {
	switch sel.name {
	case "viewer":
		return &gqlObj{typ: "User", val: &userObj{login: viewerLogin, id: viewerID}}, true
	case "__type":
		name := argString(args, "name")
		return &gqlObj{typ: "__Type", val: &typeObj{name: name}}, true
	case "__schema":
		return &gqlObj{typ: "__Schema", val: nil}, true
	}
	if typeName == "Query" {
		switch sel.name {
		case "repository":
			owner := argString(args, "owner")
			name := argString(args, "name")
			repo := ex.store.GetRepo(owner, name)
			if repo == nil {
				return nil, true
			}
			return &gqlObj{typ: "Repository", val: &repoObj{repo: repo}}, true
		case "node":
			id := argString(args, "id")
			kind, ref, ok := DecodeGlobalID(id)
			if !ok {
				return nil, true
			}
			switch kind {
			case "repo":
				parts := strings.SplitN(ref, "/", 2)
				if len(parts) != 2 {
					return nil, true
				}
				repo := ex.store.GetRepo(parts[0], parts[1])
				if repo == nil {
					return nil, true
				}
				return &gqlObj{typ: "Repository", val: &repoObj{repo: repo}}, true
			case "issue":
				owner, name, n, ok := splitRefNumber(ref)
				if !ok {
					return nil, true
				}
				repo := ex.store.GetRepo(owner, name)
				if repo == nil {
					return nil, true
				}
				is := repo.GetIssue(n)
				if is == nil {
					return nil, true
				}
				return &gqlObj{typ: "Issue", val: &issueObj{repo: repo, issue: is}}, true
			case "pr":
				owner, name, n, ok := splitRefNumber(ref)
				if !ok {
					return nil, true
				}
				repo := ex.store.GetRepo(owner, name)
				if repo == nil {
					return nil, true
				}
				pr := repo.GetPR(n)
				if pr == nil {
					return nil, true
				}
				return &gqlObj{typ: "PullRequest", val: &prObj{repo: repo, pr: pr}}, true
			}
			return nil, true
		}
		return nil, false
	}
	// Mutations.
	switch sel.name {
	case "createIssue":
		input, _ := args["input"].(map[string]any)
		repoID := argString(input, "repositoryId")
		kind, ref, ok := DecodeGlobalID(repoID)
		if !ok || kind != "repo" {
			ex.errors = append(ex.errors, map[string]any{"message": "repository not found"})
			return nil, true
		}
		parts := strings.SplitN(ref, "/", 2)
		repo := ex.store.GetRepo(parts[0], parts[1])
		if repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "repository not found"})
			return nil, true
		}
		is := ex.store.CreateIssue(repo, argString(input, "title"), argString(input, "body"), nil)
		return &gqlObj{typ: "CreateIssuePayload", val: &gqlObj{typ: "Issue", val: &issueObj{repo: repo, issue: is}}}, true
	case "closeIssue":
		input, _ := args["input"].(map[string]any)
		issueID := argString(input, "issueId")
		kind, ref, ok := DecodeGlobalID(issueID)
		if !ok || kind != "issue" {
			ex.errors = append(ex.errors, map[string]any{"message": "issue not found"})
			return nil, true
		}
		owner, name, n, ok := splitRefNumber(ref)
		if !ok {
			ex.errors = append(ex.errors, map[string]any{"message": "issue not found"})
			return nil, true
		}
		repo := ex.store.GetRepo(owner, name)
		if repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "issue not found"})
			return nil, true
		}
		is := repo.GetIssue(n)
		if is == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "issue not found"})
			return nil, true
		}
		is.State = "closed"
		is.UpdatedAt = ex.store.trialISO
		repo.rebuildIssueJSON(is)
		return &gqlObj{typ: "CloseIssuePayload", val: &gqlObj{typ: "Issue", val: &issueObj{repo: repo, issue: is}}}, true
	case "createPullRequest":
		input, _ := args["input"].(map[string]any)
		repoID := argString(input, "repositoryId")
		kind, ref, ok := DecodeGlobalID(repoID)
		if !ok || kind != "repo" {
			ex.errors = append(ex.errors, map[string]any{"message": "repository not found"})
			return nil, true
		}
		parts := strings.SplitN(ref, "/", 2)
		repo := ex.store.GetRepo(parts[0], parts[1])
		if repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "repository not found"})
			return nil, true
		}
		head := argString(input, "headRefName")
		base := argString(input, "baseRefName")
		if repo.Ref("refs/heads/"+head) == "" {
			ex.errors = append(ex.errors, map[string]any{"message": "head branch does not exist"})
			return nil, true
		}
		if repo.OpenPRForBranch(head, base) != nil {
			ex.errors = append(ex.errors, map[string]any{"message": "a pull request already exists for this head"})
			return nil, true
		}
		pr := ex.store.CreatePR(repo, argString(input, "title"), "", head, base)
		return &gqlObj{typ: "CreatePullRequestPayload", val: &gqlObj{typ: "PullRequest", val: &prObj{repo: repo, pr: pr}}}, true
	case "mergePullRequest":
		input, _ := args["input"].(map[string]any)
		prID := argString(input, "pullRequestId")
		kind, ref, ok := DecodeGlobalID(prID)
		if !ok || kind != "pr" {
			ex.errors = append(ex.errors, map[string]any{"message": "pull request not found"})
			return nil, true
		}
		owner, name, n, ok := splitRefNumber(ref)
		if !ok {
			ex.errors = append(ex.errors, map[string]any{"message": "pull request not found"})
			return nil, true
		}
		repo := ex.store.GetRepo(owner, name)
		if repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "pull request not found"})
			return nil, true
		}
		pr := repo.GetPR(n)
		if pr == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "pull request not found"})
			return nil, true
		}
		if ex.store.MergePR(repo, pr) == "" {
			ex.errors = append(ex.errors, map[string]any{"message": "pull request is conflicted"})
			return nil, true
		}
		return &gqlObj{typ: "MergePullRequestPayload", val: &gqlObj{typ: "PullRequest", val: &prObj{repo: repo, pr: pr}}}, true
	case "addComment":
		input, _ := args["input"].(map[string]any)
		subjectID := argString(input, "subjectId")
		kind, ref, ok := DecodeGlobalID(subjectID)
		if !ok || kind != "issue" {
			ex.errors = append(ex.errors, map[string]any{"message": "subject not found"})
			return nil, true
		}
		owner, name, n, ok := splitRefNumber(ref)
		if !ok {
			ex.errors = append(ex.errors, map[string]any{"message": "subject not found"})
			return nil, true
		}
		repo := ex.store.GetRepo(owner, name)
		if repo == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "subject not found"})
			return nil, true
		}
		is := repo.GetIssue(n)
		if is == nil {
			ex.errors = append(ex.errors, map[string]any{"message": "subject not found"})
			return nil, true
		}
		c := ex.store.CreateComment(repo, is, argString(input, "body"))
		edge := &gqlObj{typ: "CommentEdge", val: &gqlObj{typ: "Comment", val: &commentObj{repo: repo, issue: is, comment: c}}}
		return &gqlObj{typ: "AddCommentPayload", val: edge}, true
	}
	return nil, false
}

// ---------------------------------------------------------------- repository

func (ex *gqlExecutor) resolveRepository(repo *Repo, sel *gqlSelection, args map[string]any) (any, bool) {
	switch sel.name {
	case "id":
		return GlobalID("repo", repo.key()), true
	case "databaseId":
		return repo.ID, true
	case "name":
		return repo.Name, true
	case "nameWithOwner":
		return repo.key(), true
	case "owner":
		return &gqlObj{typ: "User", val: &userObj{login: repo.Owner, id: viewerID}}, true
	case "defaultBranchRef":
		return &gqlObj{typ: "BranchRef", val: &refObj{name: repo.DefaultBranch}}, true
	case "viewerPermission":
		return "ADMIN", true
	case "hasIssuesEnabled":
		return true, true
	case "description":
		return repo.Description, true
	case "createdAt":
		return repo.CreatedAt, true
	case "updatedAt":
		return repo.UpdatedAt, true
	case "url":
		return "https://example.invalid/" + repo.key(), true
	case "issue":
		n := argInt(args, "number", 0)
		is := repo.GetIssue(n)
		if is == nil {
			return nil, true
		}
		return &gqlObj{typ: "Issue", val: &issueObj{repo: repo, issue: is}}, true
	case "issues":
		return ex.issueConnection(repo, args), true
	case "pullRequests":
		return ex.prConnection(repo, args), true
	case "pullRequest":
		n := argInt(args, "number", 0)
		pr := repo.GetPR(n)
		if pr == nil {
			return nil, true
		}
		return &gqlObj{typ: "PullRequest", val: &prObj{repo: repo, pr: pr}}, true
	}
	return nil, false
}

// splitRefNumber splits an issue/PR global-id reference of the form
// "owner/name:number" into its parts.
func splitRefNumber(ref string) (owner, name string, n int, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", "", 0, false
	}
	num, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return "", "", 0, false
	}
	parts := strings.SplitN(ref[:i], "/", 2)
	if len(parts) != 2 {
		return "", "", 0, false
	}
	return parts[0], parts[1], num, true
}

// cursorFor encodes a page cursor for an issue/PR number.
func cursorFor(n int) string {
	return GlobalID("cursor", itoa(n))
}

func cursorNumber(cursor string) int {
	_, ref, ok := DecodeGlobalID(cursor)
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(ref)
	return n
}

// issueConnection builds the issues connection with filtering and paging.
func (ex *gqlExecutor) issueConnection(repo *Repo, args map[string]any) *gqlObj {
	first := argInt(args, "first", 30)
	after := cursorNumber(argString(args, "after"))
	var states []string
	if raw, ok := args["states"]; ok {
		if list, isList := raw.([]any); isList {
			for _, s := range list {
				if str, isStr := s.(string); isStr {
					states = append(states, strings.ToLower(str))
				}
			}
		}
	}
	desc := false
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if d, isStr := orderBy["direction"].(string); isStr && d == "DESC" {
			desc = true
		}
	}
	var filtered []*Issue
	for _, is := range repo.Issues {
		if len(states) > 0 {
			match := false
			for _, s := range states {
				if is.State == s {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		if after > 0 && is.Number <= after && !desc {
			continue
		}
		if after > 0 && is.Number >= after && desc {
			continue
		}
		filtered = append(filtered, is)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if desc {
			return filtered[i].Number > filtered[j].Number
		}
		return filtered[i].Number < filtered[j].Number
	})
	hasNext := false
	if first > 0 && len(filtered) > first {
		hasNext = true
		filtered = filtered[:first]
	}
	endCursor := ""
	if len(filtered) > 0 {
		endCursor = cursorFor(filtered[len(filtered)-1].Number)
	}
	var nodes []*gqlObj
	for _, is := range filtered {
		nodes = append(nodes, &gqlObj{typ: "Issue", val: &issueObj{repo: repo, issue: is}})
	}
	return &gqlObj{typ: "IssueConnection", val: &connObj{nodes: nodes, hasNext: hasNext, endCursor: endCursor}}
}

func (ex *gqlExecutor) prConnection(repo *Repo, args map[string]any) *gqlObj {
	first := argInt(args, "first", 30)
	after := cursorNumber(argString(args, "after"))
	var filtered []*PR
	for _, pr := range repo.PRs {
		if after > 0 && pr.Number <= after {
			continue
		}
		filtered = append(filtered, pr)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Number < filtered[j].Number })
	hasNext := false
	if first > 0 && len(filtered) > first {
		hasNext = true
		filtered = filtered[:first]
	}
	endCursor := ""
	if len(filtered) > 0 {
		endCursor = cursorFor(filtered[len(filtered)-1].Number)
	}
	var nodes []*gqlObj
	for _, pr := range filtered {
		nodes = append(nodes, &gqlObj{typ: "PullRequest", val: &prObj{repo: repo, pr: pr}})
	}
	return &gqlObj{typ: "PullRequestConnection", val: &connObj{nodes: nodes, hasNext: hasNext, endCursor: endCursor}}
}

// ---------------------------------------------------------------- issue & PR

func gqlIssueState(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func gqlPRState(pr *PR) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func (ex *gqlExecutor) resolveIssue(io *issueObj, sel *gqlSelection, args map[string]any) (any, bool) {
	is := io.issue
	switch sel.name {
	case "id":
		return GlobalID("issue", io.repo.key()+":"+itoa(is.Number)), true
	case "number":
		return is.Number, true
	case "title":
		return is.Title, true
	case "state":
		return gqlIssueState(is.State), true
	case "body":
		return is.Body, true
	case "url":
		return "https://example.invalid/" + io.repo.key() + "/issues/" + itoa(is.Number), true
	case "createdAt":
		return is.CreatedAt, true
	case "updatedAt":
		return is.UpdatedAt, true
	case "author":
		return &gqlObj{typ: "User", val: &userObj{login: viewerLogin, id: viewerID}}, true
	case "labels":
		var out []any
		for _, name := range is.Labels {
			out = append(out, name)
		}
		return out, true
	case "comments":
		return &gqlObj{typ: "IssueConnection", val: &connObj{}}, true
	}
	return nil, false
}

func (ex *gqlExecutor) resolvePR(po *prObj, sel *gqlSelection, args map[string]any) (any, bool) {
	pr := po.pr
	switch sel.name {
	case "id":
		return GlobalID("pr", po.repo.key()+":"+itoa(pr.Number)), true
	case "number":
		return pr.Number, true
	case "title":
		return pr.Title, true
	case "state":
		return gqlPRState(pr), true
	case "headRefName":
		return pr.Head, true
	case "baseRefName":
		return pr.Base, true
	case "mergeable":
		if ex.store.PRMergeable(po.repo, pr) {
			return "MERGEABLE", true
		}
		return "CONFLICTING", true
	case "merged":
		return pr.Merged, true
	case "body":
		return pr.Body, true
	case "createdAt":
		return pr.CreatedAt, true
	case "author":
		return &gqlObj{typ: "User", val: &userObj{login: viewerLogin, id: viewerID}}, true
	case "files":
		first := argInt(args, "first", 30)
		var nodes []*gqlObj
		files := pr.Files
		if first > 0 && len(files) > first {
			files = files[:first]
		}
		for _, f := range files {
			nodes = append(nodes, &gqlObj{typ: "File", val: &fileObj{path: f.Filename}})
		}
		return &gqlObj{typ: "FileConnection", val: &connObj{nodes: nodes}}, true
	}
	return nil, false
}

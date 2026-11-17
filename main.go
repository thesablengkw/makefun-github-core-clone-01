// Entry point: lifecycle client + HTTP server.
//
// Lifecycle (the S2 repair): the forge binds an ephemeral port, registers
// it with the environment server, serves until the environment reports the
// trial is done, then exits 0. The register client uses Go's standard
// http.Client with proxying disabled: Go's resolver handles DNS hostnames
// (the environment is reached as http://store:8080 in trials) and the
// standard library's response parser accepts any HTTP version token,
// including HTTP/1.0. Those two behaviours are exactly what the S1 client
// lacked, and they are what Stage 0 proves.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var reqCounter int64

func main() {
	log := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "[forge] "+format+"\n", args...)
	}

	trialUnix := parseTrialNow(os.Getenv("MAKEFUN_TRIAL_NOW"))
	store := NewStore(trialUnix)
	srv := NewForge(store, os.Getenv("MAKEFUN_SHOP_TOKEN"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log("listen: %v", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	httpServer := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 60 * time.Second,
	}

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		// No environment to register with (local grading): serve
		// indefinitely and print the port for the driver.
		log("listening on 127.0.0.1:%d (no MAKEFUN_ENV_URL)", port)
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log("serve: %v", err)
			os.Exit(1)
		}
		return
	}

	// Serve in the background while the lifecycle client registers and
	// polls; the server must be up before the environment probes it.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   10 * time.Second,
	}
	if err := registerWithEnv(client, envURL, port, log); err != nil {
		log("register: %v", err)
		httpServer.Close()
		os.Exit(1)
	}
	log("registered port %d with %s", port, envURL)

	if err := pollDone(client, envURL, log); err != nil {
		log("poll done: %v", err)
		httpServer.Close()
		os.Exit(1)
	}
	log("environment reports done; exiting 0")
	httpServer.Close()
	os.Exit(0)
}

// parseTrialNow reads the trial's now value: RFC3339, or an integer count
// of milliseconds (or seconds) since the epoch. Falls back to a fixed
// instant when absent or unparseable, so nothing ever depends on the
// wall clock.
func parseTrialNow(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1700000000
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix()
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1_000_000_000_000 {
			return n / 1000
		}
		return n
	}
	return 1700000000
}

// registerWithEnv posts {"port": N} to $ENV/register, retrying until the
// environment accepts it (200, 201, or 409 — a second register is also
// success, since the first register wins) or the attempt budget is spent.
func registerWithEnv(client *http.Client, envURL string, port int, log func(string, ...any)) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	var lastErr error
	for attempt := 1; attempt <= 60; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK, http.StatusCreated, http.StatusConflict:
				return nil
			default:
				lastErr = fmt.Errorf("register returned HTTP %d", resp.StatusCode)
			}
		}
		if attempt < 60 {
			time.Sleep(backoff(attempt))
		}
	}
	return fmt.Errorf("could not register with env after 60 attempts: %w", lastErr)
}

// pollDone polls GET $ENV/done until it reports {"done": true}.
func pollDone(client *http.Client, envURL string, log func(string, ...any)) error {
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var v struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &v) == nil && v.Done {
					return nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt*attempt) * 10 * time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
}

// ---------------------------------------------------------------- server

// Forge is the HTTP handler for every surface.
type Forge struct {
	store *Store
	token string // expected bearer token, when the trial sets one
	rest  *restHandlers
	git   *gitHandlers
	gql   *graphqlHandlers
	mcp   *mcpHandlers
}

func NewForge(store *Store, token string) *Forge {
	f := &Forge{store: store, token: token}
	f.rest = newRestHandlers(store)
	f.git = newGitHandlers(store)
	f.gql = newGraphQLHandlers(store)
	f.mcp = newMCPHandlers(store)
	return f
}

// bearerToken extracts the Bearer token from a request, or "".
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):])
	}
	return ""
}

// checkAuth enforces the bearer token rule: when the trial supplied a
// token, the presented token must match it exactly; otherwise any
// non-empty token is accepted. A missing or wrong token is 401.
// Basic auth is accepted too (real git clients speak Basic, with the
// token as the username or the password, as on github.com).
func (f *Forge) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	presented := bearerToken(r)
	if presented == "" {
		presented = f.basicToken(r)
	}
	// Advertise both schemes so clients (notably git) know how to
	// authenticate instead of failing on a bare 401.
	unauthorized := func() bool {
		w.Header().Set("WWW-Authenticate", `Basic realm="forge", Bearer`)
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	if f.token != "" {
		if presented != f.token {
			return unauthorized()
		}
		return true
	}
	if presented == "" {
		return unauthorized()
	}
	return true
}

// basicToken extracts a token from a Basic Authorization header: the
// username or the password, whichever carries it.
func (f *Forge) basicToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth[len("Basic "):]))
	if err != nil {
		return ""
	}
	user, pass, _ := strings.Cut(string(raw), ":")
	if f.token != "" {
		if user == f.token {
			return user
		}
		if pass == f.token {
			return pass
		}
		return ""
	}
	if user != "" {
		return user
	}
	return pass
}

func (f *Forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every reply carries a non-empty request id.
	id := atomic.AddInt64(&reqCounter, 1)
	w.Header().Set("X-GitHub-Request-Id", "req-"+strconv.FormatInt(id, 10))

	// A chunked request body is not a shape this forge accepts: refuse
	// it and close the connection so the client cannot keep the socket
	// alive in an ambiguous state. (Go's server already refuses
	// duplicate conflicting Content-Length headers with 400 before we
	// see the request.)
	for _, enc := range r.TransferEncoding {
		if strings.EqualFold(enc, "chunked") {
			w.Header().Set("Connection", "close")
			writeErr(w, http.StatusBadRequest, "chunked transfer encoding is not supported")
			return
		}
	}

	path := r.URL.Path
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case path == "/mcp":
		if !f.checkAuth(w, r) {
			return
		}
		f.mcp.ServeHTTP(w, r)
	case path == "/api/graphql":
		if !f.checkAuth(w, r) {
			return
		}
		f.gql.ServeHTTP(w, r)
	case strings.HasPrefix(path, "/api/v3"):
		if !f.checkAuth(w, r) {
			return
		}
		f.rest.ServeHTTP(w, r)
	case isGitPath(path):
		if !f.checkAuth(w, r) {
			return
		}
		f.git.ServeHTTP(w, r)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// isGitPath reports whether a path is a git smart-HTTP route:
// /{owner}/{repo}.git/info/refs, /{owner}/{repo}.git/git-upload-pack or
// /{owner}/{repo}.git/git-receive-pack.
func isGitPath(path string) bool {
	rest := strings.TrimPrefix(path, "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" {
		return strings.HasSuffix(parts[1], ".git")
	}
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return false
	}
	return parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack"
}

// ---------------------------------------------------------------- helpers

// writeJSON writes a JSON response with the right content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.Write(b)
}

// writeRawJSON writes pre-rendered JSON bytes.
func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// writeErr writes a JSON error body {"message": ...}.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// readJSONBody reads and decodes a JSON request body. It reports whether
// the body was present and well-formed; a malformed body is a 400.
func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, true
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return nil, false
	}
	return v, true
}

func strField(v map[string]any, key string) string {
	if s, ok := v[key].(string); ok {
		return s
	}
	return ""
}

func boolField(v map[string]any, key string) bool {
	if b, ok := v[key].(bool); ok {
		return b
	}
	return false
}

// intQuery parses a query parameter as an int with a default.
func intQuery(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return def
}

// setLinkNext writes a Link header for the next page of a paginated list.
func setLinkNext(w http.ResponseWriter, r *http.Request, page, perPage int) {
	u := *r.URL
	q := u.Query()
	q.Set("page", strconv.Itoa(page+1))
	q.Set("per_page", strconv.Itoa(perPage))
	u.RawQuery = q.Encode()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	abs := scheme + "://" + r.Host + u.String()
	w.Header().Set("Link", `<`+abs+`>; rel="next"`)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// SearchRequest is the JSON body accepted by POST /search.
//
// The request intentionally exposes only a narrow subset of grep/zgrep options.
// That keeps the API small and avoids turning this service into a generic shell
// execution endpoint.
type SearchRequest struct {
	// Keyword is passed to grep/zgrep as the pattern argument.
	Keyword string `json:"keyword"`
	// Tool selects the underlying command. Supported values are grep and zgrep.
	Tool string `json:"tool"`
	// TimeRange limits the search to files whose modification time falls within
	// the relative window, such as "1h", "30m", or "1d".
	TimeRange string `json:"time_range"`
	// MaxLines maps to grep -m and caps the number of matched lines returned.
	MaxLines int `json:"max_lines"`
	// ExtraFlags is a small allow-list of safe grep flags such as -i or -n.
	ExtraFlags []string `json:"extra_flags"`
}

// SearchResponse is returned by both successful searches and API-level failures.
type SearchResponse struct {
	// Lines contains the matched output lines from grep/zgrep.
	Lines []string `json:"lines"`
	// Count is the number of elements in Lines.
	Count int `json:"count"`
	// Command shows the exact argv that the service executed for observability.
	Command string `json:"command"`
	// Error is populated only when the request could not be fulfilled.
	Error string `json:"error,omitempty"`
}

// allowedTools is the fixed set of search executables the API may invoke.
var allowedTools = map[string]struct{}{
	"grep":  {},
	"zgrep": {},
}

// allowedFlags is the only flag surface callers may pass through to grep/zgrep.
// The list excludes options that read extra files, execute expressions, or widen
// the command surface in ways that are hard to reason about safely.
var allowedFlags = map[string]struct{}{
	"-i":                   {}, // ignore case
	"--ignore-case":        {},
	"-n":                   {}, // line number
	"--line-number":        {},
	"-c":                   {}, // count
	"--count":              {},
	"-l":                   {}, // files with matches
	"--files-with-matches": {},
	"-v":                   {}, // invert match
	"--invert-match":       {},
	"-w":                   {}, // word regexp
	"--word-regexp":        {},
	"-x":                   {}, // line regexp
	"--line-regexp":        {},
}

// runCommand is a seam for tests. Production uses exec.CommandContext; tests can
// replace it with a pure-Go fake so the package does not depend on grep existing
// on the developer machine.
var runCommand = func(ctx context.Context, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // command name and flags are validated before this call
	return cmd.Output()
}

// exitCodeError captures the only behavior runSearch needs from command errors.
// *exec.ExitError satisfies this interface, and tests can provide light fakes.
type exitCodeError interface {
	error
	ExitCode() int
}

// server stores immutable process configuration and exposes the HTTP handlers.
type server struct {
	logDir string
	mux    *http.ServeMux
}

// maxSubdirectoryDepth limits how far below the configured log root the service
// is willing to inspect files. A value of 1 means root files and files inside
// immediate child directories are searchable, while anything deeper is ignored.
const maxSubdirectoryDepth = 1

// newServer builds the request mux around a fixed log root.
func newServer(logDir string) *server {
	s := &server{logDir: logDir}

	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/health", s.handleHealth)
	s.mux = mux

	return s
}

// handleHealth reports that the process is alive and which directory it serves.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"log_dir": s.logDir,
	})
}

// handleSearch accepts a JSON request, validates it, and returns the search
// result envelope. All request validation errors are returned as JSON instead of
// using http.Error so clients always receive a consistent schema.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req SearchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	result, status, err := s.runSearch(req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// runSearch converts a validated API request into a grep/zgrep invocation.
//
// The function keeps user-controlled values as argv entries the entire time.
// There is no shell interpolation, so keywords and paths are never evaluated as
// shell syntax.
func (s *server) runSearch(req SearchRequest) (*SearchResponse, int, error) {
	if req.Tool == "" {
		req.Tool = "grep"
	}
	if _, ok := allowedTools[req.Tool]; !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("unsupported tool %q: must be one of grep, zgrep", req.Tool)
	}

	if strings.TrimSpace(req.Keyword) == "" {
		return nil, http.StatusBadRequest, errors.New("keyword must not be empty")
	}

	safeFlags := make([]string, 0, len(req.ExtraFlags))
	for _, flag := range req.ExtraFlags {
		if _, ok := allowedFlags[flag]; !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("flag %q is not allowed", flag)
		}
		safeFlags = append(safeFlags, flag)
	}

	searchTargets, err := s.resolveSearchTargets(req.TimeRange)
	if err != nil {
		if errors.Is(err, errInvalidTimeRange) {
			return nil, http.StatusBadRequest, err
		}
		return nil, http.StatusInternalServerError, err
	}
	if len(searchTargets) == 0 {
		return &SearchResponse{Lines: []string{}, Count: 0, Command: ""}, http.StatusOK, nil
	}

	args := buildSearchArgs(req, safeFlags, searchTargets)
	commandString := req.Tool + " " + strings.Join(args, " ")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	output, err := runCommand(ctx, req.Tool, args)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, http.StatusGatewayTimeout, errors.New("search command timed out")
		}

		var exitErr exitCodeError
		if errors.As(err, &exitErr) {
			if exitErr.ExitCode() == 1 {
				return &SearchResponse{Lines: []string{}, Count: 0, Command: commandString}, http.StatusOK, nil
			}

			var execErr *exec.ExitError
			if errors.As(err, &execErr) && len(execErr.Stderr) > 0 {
				return nil, http.StatusInternalServerError, fmt.Errorf("command failed (exit %d): %s", exitErr.ExitCode(), string(execErr.Stderr))
			}
			return nil, http.StatusInternalServerError, fmt.Errorf("command failed (exit %d): %v", exitErr.ExitCode(), err)
		}

		return nil, http.StatusInternalServerError, fmt.Errorf("command error: %v", err)
	}

	lines := splitOutputLines(output)
	return &SearchResponse{
		Lines:   lines,
		Count:   len(lines),
		Command: commandString,
	}, http.StatusOK, nil
}

var errInvalidTimeRange = errors.New("invalid time_range")

// resolveSearchTargets returns the explicit file list that grep/zgrep may scan.
// Every search shares the same maximum depth so the service never traverses
// beyond one child directory below the configured log root.
func (s *server) resolveSearchTargets(timeRange string) ([]string, error) {
	if timeRange == "" {
		paths, err := collectSearchableFiles(s.logDir, nil)
		if err != nil {
			return nil, fmt.Errorf("collect searchable files: %w", err)
		}
		return paths, nil
	}

	duration, err := parseDuration(timeRange)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidTimeRange, err)
	}

	cutoff := time.Now().Add(-duration)
	paths, err := collectSearchableFiles(s.logDir, &cutoff)
	if err != nil {
		return nil, fmt.Errorf("collect files for time range: %w", err)
	}

	return paths, nil
}

// buildSearchArgs assembles the final grep/zgrep argv slice.
//
// The service always passes explicit file paths rather than directory roots.
// That keeps the effective search depth under application control instead of
// delegating recursion policy to grep/zgrep.
func buildSearchArgs(req SearchRequest, safeFlags, searchTargets []string) []string {
	args := make([]string, 0, len(safeFlags)+len(searchTargets)+5)
	if req.MaxLines > 0 {
		args = append(args, "-m", fmt.Sprintf("%d", req.MaxLines))
	}
	args = append(args, safeFlags...)
	args = append(args, "--", req.Keyword)
	args = append(args, searchTargets...)
	return args
}

// splitOutputLines turns grep stdout into a JSON-friendly slice.
func splitOutputLines(output []byte) []string {
	raw := strings.TrimRight(string(output), "\n")
	if raw == "" {
		return []string{}
	}
	return strings.Split(raw, "\n")
}

// parseDuration extends time.ParseDuration with a single extra suffix: "d" for
// whole days. This matches the API examples while still delegating standard unit
// parsing to the Go standard library.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		hours, err := time.ParseDuration(fmt.Sprintf("%sh", daysStr))
		if err != nil {
			return 0, err
		}
		return hours * 24, nil
	}

	return time.ParseDuration(s)
}

// collectSearchableFiles walks the configured log root and returns regular files
// that satisfy the service's traversal and optional time-range policy.
//
// Important details:
//   - the walk starts from the configured root, so results stay inside that tree
//   - traversal stops after one child directory level below the root
//   - only regular files are included; directories, symlinks, and special files
//     are skipped because they are poor grep/zgrep targets
//   - when modifiedSince is not nil, only files with mtime at or after the cutoff
//     are returned
//   - the returned list is sorted for stable command strings and deterministic tests
func collectSearchableFiles(logDir string, modifiedSince *time.Time) ([]string, error) {
	absLogDir, err := filepath.Abs(logDir)
	if err != nil {
		return nil, fmt.Errorf("resolve log directory: %w", err)
	}

	paths := make([]string, 0, 32)
	err = filepath.WalkDir(absLogDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relativePath, err := filepath.Rel(absLogDir, path)
		if err != nil {
			return fmt.Errorf("compute relative path for %q: %w", path, err)
		}

		if entry.IsDir() {
			if relativePath == "." {
				return nil
			}
			if relativeParentDepth(relativePath) > maxSubdirectoryDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if relativeParentDepth(relativePath) > maxSubdirectoryDepth {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", path, err)
		}
		if modifiedSince != nil && info.ModTime().Before(*modifiedSince) {
			return nil
		}

		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(paths)
	return paths, nil
}

// relativeParentDepth reports how many directory levels below the walk root a
// path's parent directory sits. Examples:
// - "root.log" => 0
// - "api/root.log" => 1
// - "api/2026/root.log" => 2
func relativeParentDepth(relativePath string) int {
	if relativePath == "." || relativePath == "" {
		return 0
	}
	return strings.Count(relativePath, string(os.PathSeparator))
}

// writeError keeps API errors in the same JSON envelope shape as success cases.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(SearchResponse{Error: msg})
}

// main validates startup configuration, creates the HTTP server, and performs a
// graceful shutdown when the process receives SIGINT or SIGTERM.
func main() {
	logDir := flag.String("log-dir", "", "path to the log directory to search (required)")
	addr := flag.String("addr", ":9999", "address to listen on (default :9999)")
	flag.Parse()

	if *logDir == "" {
		fmt.Fprintln(os.Stderr, "error: --log-dir is required")
		flag.Usage()
		os.Exit(1)
	}

	info, err := os.Stat(*logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot access log directory %q: %v\n", *logDir, err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %q is not a directory\n", *logDir)
		os.Exit(1)
	}

	s := newServer(*logDir)
	httpServer := &http.Server{
		Addr:         *addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("log-tools server listening on %s (log dir: %s)", *addr, *logDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("server stopped")
}

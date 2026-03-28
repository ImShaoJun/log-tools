package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SearchRequest represents the incoming search request payload.
type SearchRequest struct {
	// Keyword is the search pattern passed to grep/zgrep.
	Keyword string `json:"keyword"`
	// Tool selects the underlying command: "grep" or "zgrep".
	Tool string `json:"tool"`
	// TimeRange is a relative time duration string (e.g., "1h", "30m", "1d").
	// If set, it's used with the server's --log-name-format to find log files.
	TimeRange string `json:"time_range"`
	// MaxLines limits the number of matching lines returned (maps to grep -m).
	MaxLines int `json:"max_lines"`
	// ExtraFlags are optional additional flags forwarded to grep/zgrep (e.g. ["-i", "-n"]).
	// Only the safe allow-listed flags are accepted.
	ExtraFlags []string `json:"extra_flags"`
}

// SearchResponse is the JSON envelope returned to callers.
type SearchResponse struct {
	// Lines contains the matched output lines.
	Lines []string `json:"lines"`
	// Count is the number of matched lines.
	Count int `json:"count"`
	// Command is the full command that was executed (for transparency / debugging).
	Command string `json:"command"`
	// Error carries an error message when the request could not be fulfilled.
	Error string `json:"error,omitempty"`
}

// allowedTools is the set of supported search utilities.
var allowedTools = map[string]struct{}{
	"grep":  {},
	"zgrep": {},
}

// allowedFlags is an allow-list of safe grep/zgrep flags callers may request.
var allowedFlags = map[string]struct{}{
	"-i":           {}, // ignore case
	"--ignore-case": {},
	"-n":           {}, // line number
	"--line-number": {},
	"-c":           {}, // count
	"--count":      {},
	"-l":           {}, // files with matches
	"--files-with-matches": {},
	"-v":           {}, // invert match
	"--invert-match": {},
	"-w":           {}, // word regexp
	"--word-regexp": {},
	"-x":           {}, // line regexp
	"--line-regexp": {},
}

// server holds the application-wide configuration.
type server struct {
	logDir        string
	logNameFormat string
	mux           *http.ServeMux
}

func newServer(logDir, logNameFormat string) *server {
	s := &server{
		logDir:        logDir,
		logNameFormat: logNameFormat,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/health", s.handleHealth)
	s.mux = mux
	return s
}

// handleHealth is a simple liveness probe.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "log_dir": s.logDir})
}

// handleSearch processes POST /search requests.
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

// runSearch validates the request, builds the command and executes it.
func (s *server) runSearch(req SearchRequest) (*SearchResponse, int, error) {
	// Validate tool.
	if req.Tool == "" {
		req.Tool = "grep"
	}
	if _, ok := allowedTools[req.Tool]; !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("unsupported tool %q: must be one of grep, zgrep", req.Tool)
	}

	// Validate keyword.
	if strings.TrimSpace(req.Keyword) == "" {
		return nil, http.StatusBadRequest, errors.New("keyword must not be empty")
	}

	// Validate extra flags against the allow-list.
	var safeFlags []string
	for _, f := range req.ExtraFlags {
		if _, ok := allowedFlags[f]; !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("flag %q is not allowed", f)
		}
		safeFlags = append(safeFlags, f)
	}

	// Determine search targets.
	var searchTargets []string
	if req.TimeRange != "" {
		if s.logNameFormat == "" {
			return nil, http.StatusBadRequest, errors.New("time_range search requires the --log-name-format server flag to be set")
		}
		duration, err := parseDuration(req.TimeRange)
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid time_range: %v", err)
		}
		searchTargets, err = generateFilePaths(s.logDir, s.logNameFormat, duration)
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("could not generate file paths for time range: %v", err)
		}
		if len(searchTargets) == 0 {
			// No files matched the time range, return empty result.
			return &SearchResponse{Lines: []string{}, Count: 0, Command: ""}, http.StatusOK, nil
		}
	} else {
		// Default to searching the whole log directory recursively.
		searchTargets = []string{s.logDir}
	}

	// Build the argument list.
	// We use -r (recursive) so that the log directory can contain sub-directories.
	args := []string{"-r"}
	if req.MaxLines > 0 {
		args = append(args, "-m", fmt.Sprintf("%d", req.MaxLines))
	}
	args = append(args, safeFlags...)
	// Pass keyword and paths as separate arguments to avoid shell injection.
	args = append(args, "--")
	args = append(args, req.Keyword)
	args = append(args, searchTargets...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Tool, args...) //nolint:gosec // tool is allow-listed; args are not shell-expanded
	output, err := cmd.Output()

	// Build human-readable command string for the response.
	cmdStr := req.Tool + " " + strings.Join(args, " ")

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, http.StatusGatewayTimeout, errors.New("search command timed out")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Exit code 1 from grep means "no matches found" – not an error.
			if exitErr.ExitCode() == 1 {
				return &SearchResponse{Lines: []string{}, Count: 0, Command: cmdStr}, http.StatusOK, nil
			}
			// Exit code 2 or other: real error.
			return nil, http.StatusInternalServerError, fmt.Errorf("command failed (exit %d): %s", exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return nil, http.StatusInternalServerError, fmt.Errorf("command error: %v", err)
	}

	// Split output into lines, dropping the trailing empty entry.
	raw := strings.TrimRight(string(output), "\n")
	var lines []string
	if raw != "" {
		lines = strings.Split(raw, "\n")
	} else {
		lines = []string{}
	}

	return &SearchResponse{
		Lines:   lines,
		Count:   len(lines),
		Command: cmdStr,
	}, http.StatusOK, nil
}

// parseDuration converts a string like "1d", "2h", "30m" into a time.Duration.
// It extends time.ParseDuration by supporting "d" for days.
func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		days, err := time.ParseDuration(fmt.Sprintf("%sh", daysStr))
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
	return time.ParseDuration(s)
}

// generateFilePaths creates a list of file paths based on a log name format and time duration.
// It works back from the current time.
func generateFilePaths(logDir, format string, duration time.Duration) ([]string, error) {
	// 1. Convert our format to Go's layout string.
	layout := strings.NewReplacer("%Y", "2006", "%m", "01", "%d", "02", "%H", "15").Replace(format)

	// 2. Determine the granularity of the format (daily or hourly).
	var step time.Duration
	if strings.Contains(format, "%H") {
		step = time.Hour
	} else {
		step = 24 * time.Hour
	}

	// 3. Iterate from now back to the start time, generating file names.
	pathSet := make(map[string]struct{})
	now := time.Now()
	startTime := now.Add(-duration)

	for t := now; t.After(startTime) || t.Equal(startTime); t = t.Add(-step) {
		// Format the time according to the layout to get the file name.
		fileName := t.Format(layout)
		fullPath := filepath.Join(logDir, fileName)

		// Ensure the generated path is clean and within the log directory.
		cleanPath := filepath.Clean(fullPath)
		absLogDir, err := filepath.Abs(logDir)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute log directory: %v", err)
		}
		absCleanPath, err := filepath.Abs(cleanPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute search path: %v", err)
		}
		if !strings.HasPrefix(absCleanPath, absLogDir) {
			// This should theoretically not happen if we build paths correctly, but as a safeguard.
			continue
		}

		pathSet[cleanPath] = struct{}{}
	}

	// Convert the set to a slice.
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}

	return paths, nil
}


func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(SearchResponse{Error: msg})
}

func main() {
	logDir := flag.String("log-dir", "", "path to the log directory to search (required)")
	addr := flag.String("addr", ":9999", "address to listen on (default :9999)")
	logNameFormat := flag.String("log-name-format", "", "log file name format for time-based search (e.g. 'app-%Y-%m-%d.log')")
	flag.Parse()

	if *logDir == "" {
		fmt.Fprintln(os.Stderr, "error: --log-dir is required")
		flag.Usage()
		os.Exit(1)
	}

	// Ensure the log directory exists.
	info, err := os.Stat(*logDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot access log directory %q: %v\n", *logDir, err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %q is not a directory\n", *logDir)
		os.Exit(1)
	}

	s := newServer(*logDir, *logNameFormat)

	httpServer := &http.Server{
		Addr:         *addr,
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("log-tools server listening on %s (log dir: %s, name format: %q)", *addr, *logDir, *logNameFormat)
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

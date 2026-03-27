package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupLogDir creates a temporary directory with sample log files for tests.
func setupLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Plain-text log file.
	err := os.WriteFile(filepath.Join(dir, "app.log"), []byte(
		"2024-01-01 INFO  starting application\n"+
			"2024-01-01 ERROR something went wrong\n"+
			"2024-01-01 INFO  shutting down\n",
	), 0644)
	if err != nil {
		t.Fatalf("create app.log: %v", err)
	}

	// Another log file.
	err = os.WriteFile(filepath.Join(dir, "access.log"), []byte(
		"GET /health 200\n"+
			"POST /search 200\n"+
			"GET /missing 404\n",
	), 0644)
	if err != nil {
		t.Fatalf("create access.log: %v", err)
	}

	return dir
}

func postSearch(t *testing.T, s *server, body SearchRequest) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) SearchResponse {
	t.Helper()
	var resp SearchResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// TestHealthEndpoint verifies the /health handler.
func TestHealthEndpoint(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// TestSearchMatchFound verifies that grep returns matching lines.
func TestSearchMatchFound(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "ERROR", Tool: "grep"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Count == 0 {
		t.Fatal("expected at least one match")
	}
	for _, line := range resp.Lines {
		if line == "" {
			t.Error("unexpected empty line in results")
		}
	}
}

// TestSearchNoMatch verifies that a missing keyword returns an empty result set (not an error).
func TestSearchNoMatch(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "NOMATCH_XYZ_UNIQUE", Tool: "grep"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Count != 0 {
		t.Fatalf("expected 0 matches, got %d", resp.Count)
	}
}

// TestSearchDefaultTool verifies that an empty tool field defaults to grep.
func TestSearchDefaultTool(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodeResponse(t, rr)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if resp.Count == 0 {
		t.Fatal("expected matches")
	}
}

// TestSearchCaseInsensitiveFlag verifies the -i flag.
func TestSearchCaseInsensitiveFlag(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	// Without -i, lowercase "error" should not match "ERROR".
	rr := postSearch(t, s, SearchRequest{Keyword: "error", Tool: "grep"})
	resp := decodeResponse(t, rr)
	withoutFlag := resp.Count

	// With -i, it should match.
	rr = postSearch(t, s, SearchRequest{Keyword: "error", Tool: "grep", ExtraFlags: []string{"-i"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp = decodeResponse(t, rr)
	if resp.Count <= withoutFlag {
		t.Fatalf("expected more matches with -i flag (got %d, without flag: %d)", resp.Count, withoutFlag)
	}
}

// TestSearchDisallowedFlag verifies that unknown flags are rejected.
func TestSearchDisallowedFlag(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "grep", ExtraFlags: []string{"--exec-something"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	resp := decodeResponse(t, rr)
	if resp.Error == "" {
		t.Fatal("expected error message")
	}
}

// TestSearchInvalidTool verifies that an unsupported tool is rejected.
func TestSearchInvalidTool(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "bash"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSearchEmptyKeyword verifies that an empty keyword is rejected.
func TestSearchEmptyKeyword(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Tool: "grep"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSearchMethodNotAllowed verifies that GET /search is rejected.
func TestSearchMethodNotAllowed(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// TestSearchFilePattern verifies that file_pattern restricts search scope.
func TestSearchFilePattern(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	// "404" only appears in access.log.
	rr := postSearch(t, s, SearchRequest{Keyword: "404", Tool: "grep", FilePattern: "access.log"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodeResponse(t, rr)
	if resp.Count == 0 {
		t.Fatal("expected matches in access.log")
	}

	// Searching for "404" in app.log should return no matches.
	rr = postSearch(t, s, SearchRequest{Keyword: "404", Tool: "grep", FilePattern: "app.log"})
	resp = decodeResponse(t, rr)
	if resp.Count != 0 {
		t.Fatalf("expected 0 matches in app.log, got %d", resp.Count)
	}
}

// TestSearchPathTraversal verifies that directory traversal in file_pattern is prevented.
func TestSearchPathTraversal(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	// Attempt path traversal in file pattern.
	rr := postSearch(t, s, SearchRequest{Keyword: "root", Tool: "grep", FilePattern: "../../etc/passwd"})
	// The path should be sanitized so the resolved path stays within logDir.
	// Either it returns empty results (file not found) or a proper response—
	// but it must NOT return an internal error exposing system files.
	resp := decodeResponse(t, rr)
	// If the traversal were successful it would contain entries like "root:x:0:0".
	for _, line := range resp.Lines {
		if bytes.Contains([]byte(line), []byte("root:x:")) {
			t.Fatal("path traversal succeeded – this is a security vulnerability")
		}
	}
}

// TestCommandFieldPopulated verifies that the Command field is always returned.
func TestCommandFieldPopulated(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "grep"})
	resp := decodeResponse(t, rr)
	if resp.Command == "" {
		t.Fatal("Command field should not be empty")
	}
}

// TestSearchFilePatternPrefix verifies that file_pattern cannot start with '..' or '/'.
func TestSearchFilePatternPrefix(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	// Test ".." prefix
	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", FilePattern: "../etc/passwd"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for '..' prefix, got %d", rr.Code)
	}

	// Test "/" prefix
	rr = postSearch(t, s, SearchRequest{Keyword: "INFO", FilePattern: "/etc/passwd"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for '/' prefix, got %d", rr.Code)
	}
}

// TestSearchSubdirAndTraversal verifies that subdirectory searches work while preventing traversal.
func TestSearchSubdirAndTraversal(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir)

	// Setup a subdirectory
	subdir := filepath.Join(dir, "logs")
	err := os.Mkdir(subdir, 0755)
	if err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	err = os.WriteFile(filepath.Join(subdir, "inner.log"), []byte("MATCH_ME\n"), 0644)
	if err != nil {
		t.Fatalf("create inner.log: %v", err)
	}

	// Test valid subdirectory search
	rr := postSearch(t, s, SearchRequest{Keyword: "MATCH_ME", FilePattern: "logs/inner.log"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for subdir search, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp.Count != 1 {
		t.Fatalf("expected 1 match in subdir, got %d", resp.Count)
	}

	// Test traversal attempt: "logs/../../" (stays inside if using filepath.Base, but we want it to stay in logDir)
	// The prefix check caught it at start, but let's test something that bypasses it if possible.
	// Actually "a/../../etc/passwd" doesn't start with ".." or "/".
	rr = postSearch(t, s, SearchRequest{Keyword: "root", FilePattern: "logs/../../etc/passwd"})
	if rr.Code == http.StatusOK {
		resp = decodeResponse(t, rr)
		for _, line := range resp.Lines {
			if strings.Contains(line, "root:x:") {
				t.Fatal("path traversal succeeded")
			}
		}
	} else if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		// Either it should be blocked (400) or not found (maybe 200 with 0 matches).
		// Currently filepath.Base("logs/../../etc/passwd") is "passwd", which might exist in logDir.
	}
}

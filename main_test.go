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
	"time"
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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

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
	s := newServer(dir, "")

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "bash"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSearchEmptyKeyword verifies that an empty keyword is rejected.
func TestSearchEmptyKeyword(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir, "")

	rr := postSearch(t, s, SearchRequest{Tool: "grep"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestSearchMethodNotAllowed verifies that GET /search is rejected.
func TestSearchMethodNotAllowed(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir, "")

	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// TestCommandFieldPopulated verifies that the Command field is always returned.
func TestCommandFieldPopulated(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir, "")

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "grep"})
	resp := decodeResponse(t, rr)
	if resp.Command == "" {
		t.Fatal("Command field should not be empty")
	}
}

// TestParseDuration checks the custom duration parser.
func TestParseDuration(t *testing.T) {
	tests := []struct {
		in       string
		want     time.Duration
		errIsNil bool
	}{
		{"1h", time.Hour, true},
		{"30m", 30 * time.Minute, true},
		{"1d", 24 * time.Hour, true},
		{"2d", 48 * time.Hour, true},
		{"", 0, false},
		{"1", 0, false},
		{"1x", 0, false},
	}

	for _, tc := range tests {
		got, err := parseDuration(tc.in)
		if (err == nil) != tc.errIsNil {
			t.Errorf("parseDuration(%q): got err %v, want err? %v", tc.in, err, tc.errIsNil)
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q): got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGenerateFilePaths verifies the file path generation logic.
func TestGenerateFilePaths(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	format := "log-%Y-%m-%d.log"
	layout := "log-2006-01-02.log"

	// Create some dummy files.
	todayFile := now.Format(layout)
	yesterdayFile := now.AddDate(0, 0, -1).Format(layout)
	twoDaysAgoFile := now.AddDate(0, 0, -2).Format(layout)
	if err := os.WriteFile(filepath.Join(dir, todayFile), []byte("today"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, yesterdayFile), []byte("yesterday"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, twoDaysAgoFile), []byte("two days ago"), 0644); err != nil {
		t.Fatal(err)
	}

	// Test case: 25 hours -> should find today's and yesterday's file.
	paths, err := generateFilePaths(dir, format, 25*time.Hour)
	if err != nil {
		t.Fatalf("generateFilePaths error: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}

	// Check that the two files are today's and yesterday's.
	foundToday := false
	foundYesterday := false
	for _, p := range paths {
		if strings.HasSuffix(p, todayFile) {
			foundToday = true
		}
		if strings.HasSuffix(p, yesterdayFile) {
			foundYesterday = true
		}
	}
	if !foundToday || !foundYesterday {
		t.Errorf("expected to find today's and yesterday's log files, got %v", paths)
	}
}

// TestSearchMaxLines verifies the max_lines parameter.
func TestSearchMaxLines(t *testing.T) {
	dir := setupLogDir(t)
	s := newServer(dir, "") // No time format needed for this test.

	// There are two "INFO" lines in app.log.
	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", MaxLines: 1})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResponse(t, rr)
	if resp.Count != 1 {
		t.Fatalf("expected 1 match with max_lines=1, got %d", resp.Count)
	}
}

// TestSearchTimeRange verifies time-based searching.
func TestSearchTimeRange(t *testing.T) {
	dir := t.TempDir()
	format := "app-%Y-%m-%d.log"
	layout := "app-2006-01-02.log"
	now := time.Now()

	todayFile := now.Format(layout)
	yesterdayFile := now.AddDate(0, 0, -1).Format(layout)

	err := os.WriteFile(filepath.Join(dir, todayFile), []byte("TODAY_CONTENT"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(dir, yesterdayFile), []byte("YESTERDAY_CONTENT"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	s := newServer(dir, format)

	// Search last hour, should only find today's file.
	rr := postSearch(t, s, SearchRequest{Keyword: "CONTENT", TimeRange: "1h"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp := decodeResponse(t, rr)
	if resp.Count != 1 {
		t.Fatalf("expected 1 match for 1h range, got %d", resp.Count)
	}
	if !strings.Contains(resp.Lines[0], "TODAY_CONTENT") {
		t.Errorf("expected to find today's content, got %s", resp.Lines[0])
	}
	if !strings.Contains(resp.Command, todayFile) {
		t.Errorf("command should have contained today's file: %s", resp.Command)
	}
	if strings.Contains(resp.Command, yesterdayFile) {
		t.Errorf("command should NOT have contained yesterday's file: %s", resp.Command)
	}

	// Search last 25 hours, should find both.
	rr = postSearch(t, s, SearchRequest{Keyword: "CONTENT", TimeRange: "25h"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	resp = decodeResponse(t, rr)
	if resp.Count != 2 {
		t.Fatalf("expected 2 matches for 25h range, got %d", resp.Count)
	}
}

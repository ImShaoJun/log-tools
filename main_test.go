package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeExitError lets tests emulate grep exit status 1 ("no matches") without
// needing a real os/exec process.
type fakeExitError struct {
	code int
}

func (e fakeExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

func (e fakeExitError) ExitCode() int {
	return e.code
}

type fakeSearchOptions struct {
	recursive  bool
	ignoreCase bool
	maxLines   int
}

// setupLogDir creates a temporary directory with sample log files for tests.
func setupLogDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.log"), ""+
		"2024-01-01 INFO  starting application\n"+
		"2024-01-01 ERROR something went wrong\n"+
		"2024-01-01 INFO  shutting down\n")
	writeFile(t, filepath.Join(dir, "access.log"), ""+
		"GET /health 200\n"+
		"POST /search 200\n"+
		"GET /missing 404\n")

	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func setModTime(t *testing.T, path string, ts time.Time) {
	t.Helper()

	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
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

// useFakeSearchCommand swaps the production command runner with a small pure-Go
// grep emulator so tests remain portable across Windows and Linux.
func useFakeSearchCommand(t *testing.T) {
	t.Helper()

	original := runCommand
	runCommand = fakeSearchCommand
	t.Cleanup(func() {
		runCommand = original
	})
}

func fakeSearchCommand(_ context.Context, name string, args []string) ([]byte, error) {
	if name != "grep" && name != "zgrep" {
		return nil, fmt.Errorf("unexpected tool %q", name)
	}

	opts, keyword, targets, err := parseFakeSearchArgs(args)
	if err != nil {
		return nil, err
	}

	files, err := expandFakeTargets(targets, opts.recursive)
	if err != nil {
		return nil, err
	}

	matches := make([]string, 0, 8)
	needle := keyword
	if opts.ignoreCase {
		needle = strings.ToLower(keyword)
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}

		for _, line := range splitTestLines(string(data)) {
			haystack := line
			if opts.ignoreCase {
				haystack = strings.ToLower(line)
			}
			if !strings.Contains(haystack, needle) {
				continue
			}

			matches = append(matches, file+":"+line)
			if opts.maxLines > 0 && len(matches) >= opts.maxLines {
				return []byte(strings.Join(matches, "\n")), nil
			}
		}
	}

	if len(matches) == 0 {
		return nil, fakeExitError{code: 1}
	}
	return []byte(strings.Join(matches, "\n")), nil
}

func parseFakeSearchArgs(args []string) (fakeSearchOptions, string, []string, error) {
	var opts fakeSearchOptions
	separator := -1

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-r":
			opts.recursive = true
		case "-m":
			if i+1 >= len(args) {
				return opts, "", nil, errorsForTests("missing value for -m")
			}
			value, err := strconv.Atoi(args[i+1])
			if err != nil {
				return opts, "", nil, err
			}
			opts.maxLines = value
			i++
		case "-i", "--ignore-case":
			opts.ignoreCase = true
		case "-n", "--line-number", "-c", "--count", "-l", "--files-with-matches", "-v", "--invert-match", "-w", "--word-regexp", "-x", "--line-regexp":
			// These flags are allow-listed by the API. The tests in this package do
			// not assert their behavior, so the fake runner only needs to accept them.
		case "--":
			separator = i
			i = len(args)
		default:
			return opts, "", nil, errorsForTests("unexpected arg " + args[i])
		}
	}

	if separator == -1 || separator+2 > len(args) {
		return opts, "", nil, errorsForTests("missing pattern or search targets")
	}

	return opts, args[separator+1], args[separator+2:], nil
}

func expandFakeTargets(targets []string, recursive bool) ([]string, error) {
	files := make([]string, 0, len(targets))

	for _, target := range targets {
		info, err := os.Stat(target)
		if err != nil {
			return nil, err
		}

		if info.IsDir() {
			if !recursive {
				return nil, errorsForTests("directory target requires recursive mode")
			}

			err := filepath.Walk(target, func(path string, fileInfo os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if fileInfo.IsDir() {
					return nil
				}
				files = append(files, path)
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}

		files = append(files, target)
	}

	sort.Strings(files)
	return files, nil
}

func splitTestLines(content string) []string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func errorsForTests(msg string) error {
	return errors.New(msg)
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
	useFakeSearchCommand(t)

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

// TestSearchNoMatch verifies that a missing keyword returns an empty result set.
func TestSearchNoMatch(t *testing.T) {
	useFakeSearchCommand(t)

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
	useFakeSearchCommand(t)

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
	useFakeSearchCommand(t)

	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "error", Tool: "grep"})
	resp := decodeResponse(t, rr)
	withoutFlag := resp.Count

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

// TestCommandFieldPopulated verifies that the Command field is always returned
// when a command is actually executed.
func TestCommandFieldPopulated(t *testing.T) {
	useFakeSearchCommand(t)

	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", Tool: "grep"})
	resp := decodeResponse(t, rr)
	if resp.Command == "" {
		t.Fatal("command field should not be empty")
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

// TestCollectSearchableFiles verifies mtime filtering and the depth limit.
func TestCollectSearchableFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	recentFile := filepath.Join(dir, "recent.log")
	oldFile := filepath.Join(dir, "old.log")
	nestedRecentFile := filepath.Join(dir, "nested", "recent-nested.log")
	deepRecentFile := filepath.Join(dir, "nested", "deep", "too-deep.log")

	writeFile(t, recentFile, "recent")
	writeFile(t, oldFile, "old")
	writeFile(t, nestedRecentFile, "nested recent")
	writeFile(t, deepRecentFile, "deep recent")

	setModTime(t, recentFile, now.Add(-20*time.Minute))
	setModTime(t, oldFile, now.Add(-3*time.Hour))
	setModTime(t, nestedRecentFile, now.Add(-10*time.Minute))
	setModTime(t, deepRecentFile, now.Add(-5*time.Minute))

	cutoff := now.Add(-1 * time.Hour)
	paths, err := collectSearchableFiles(dir, &cutoff)
	if err != nil {
		t.Fatalf("collectSearchableFiles error: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}
	if !slicesContain(paths, recentFile) {
		t.Fatalf("expected %s to be returned, got %v", recentFile, paths)
	}
	if !slicesContain(paths, nestedRecentFile) {
		t.Fatalf("expected %s to be returned, got %v", nestedRecentFile, paths)
	}
	if slicesContain(paths, oldFile) {
		t.Fatalf("did not expect %s to be returned, got %v", oldFile, paths)
	}
	if slicesContain(paths, deepRecentFile) {
		t.Fatalf("did not expect %s to be returned, got %v", deepRecentFile, paths)
	}
}

// TestSearchMaxLines verifies the max_lines parameter.
func TestSearchMaxLines(t *testing.T) {
	useFakeSearchCommand(t)

	dir := setupLogDir(t)
	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "INFO", MaxLines: 1})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Count != 1 {
		t.Fatalf("expected 1 match with max_lines=1, got %d", resp.Count)
	}
}

// TestSearchTimeRange verifies that time_range filters files by modification time.
func TestSearchTimeRange(t *testing.T) {
	useFakeSearchCommand(t)

	dir := t.TempDir()
	now := time.Now()

	recentFile := filepath.Join(dir, "today.log")
	oldFile := filepath.Join(dir, "yesterday.log")
	deepRecentFile := filepath.Join(dir, "archive", "2026", "too-deep.log")

	writeFile(t, recentFile, "TODAY_CONTENT\n")
	writeFile(t, oldFile, "YESTERDAY_CONTENT\n")
	writeFile(t, deepRecentFile, "DEEP_CONTENT\n")
	setModTime(t, recentFile, now.Add(-20*time.Minute))
	setModTime(t, oldFile, now.Add(-26*time.Hour))
	setModTime(t, deepRecentFile, now.Add(-15*time.Minute))

	s := newServer(dir)

	rr := postSearch(t, s, SearchRequest{Keyword: "CONTENT", TimeRange: "1h"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Count != 1 {
		t.Fatalf("expected 1 match for 1h range, got %d", resp.Count)
	}
	if !strings.Contains(resp.Lines[0], "TODAY_CONTENT") {
		t.Errorf("expected to find recent content, got %s", resp.Lines[0])
	}
	if !strings.Contains(resp.Command, recentFile) {
		t.Errorf("command should contain the recent file: %s", resp.Command)
	}
	if strings.Contains(resp.Command, oldFile) {
		t.Errorf("command should not contain the old file: %s", resp.Command)
	}
	if strings.Contains(resp.Command, deepRecentFile) {
		t.Errorf("command should not contain the too-deep file: %s", resp.Command)
	}

	rr = postSearch(t, s, SearchRequest{Keyword: "CONTENT", TimeRange: "30h"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp = decodeResponse(t, rr)
	if resp.Count != 2 {
		t.Fatalf("expected 2 matches for 30h range, got %d", resp.Count)
	}
}

// TestSearchDepthLimit verifies that regular searches stop after one child
// directory level even when deeper files also contain matches.
func TestSearchDepthLimit(t *testing.T) {
	useFakeSearchCommand(t)

	dir := t.TempDir()
	rootFile := filepath.Join(dir, "root.log")
	childFile := filepath.Join(dir, "api", "child.log")
	deepFile := filepath.Join(dir, "api", "2026", "deep.log")

	writeFile(t, rootFile, "DEPTH_MATCH\n")
	writeFile(t, childFile, "DEPTH_MATCH\n")
	writeFile(t, deepFile, "DEPTH_MATCH\n")

	s := newServer(dir)
	rr := postSearch(t, s, SearchRequest{Keyword: "DEPTH_MATCH"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeResponse(t, rr)
	if resp.Count != 2 {
		t.Fatalf("expected 2 matches with depth limit, got %d: %+v", resp.Count, resp.Lines)
	}
	if !strings.Contains(resp.Command, rootFile) {
		t.Fatalf("command should contain %s: %s", rootFile, resp.Command)
	}
	if !strings.Contains(resp.Command, childFile) {
		t.Fatalf("command should contain %s: %s", childFile, resp.Command)
	}
	if strings.Contains(resp.Command, deepFile) {
		t.Fatalf("command should not contain %s: %s", deepFile, resp.Command)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

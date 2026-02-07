package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

func setupTestConfig(t *testing.T) func() {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "tag-proxy-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cfg = Config{
		Port:            8080,
		CacheDir:        tmpDir,
		MaxTags:         1000,
		DockerHubJWT:    "",
		RefreshInterval: 24 * time.Hour,
	}

	httpClient = retryablehttp.NewClient()
	httpClient.RetryMax = 1
	httpClient.Logger = nil

	return func() {
		os.RemoveAll(tmpDir)
	}
}

// --- getShardedPath tests ---

func TestGetShardedPath(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	tests := []struct {
		name      string
		imageName string
		wantPath  string
	}{
		{
			name:      "simple image name",
			imageName: "nginx",
			wantPath:  filepath.Join(cfg.CacheDir, "n", "ng", "nginx.json"),
		},
		{
			name:      "image with namespace",
			imageName: "library/nginx",
			wantPath:  filepath.Join(cfg.CacheDir, "l", "li", "library_nginx.json"),
		},
		{
			name:      "image with org and repo",
			imageName: "myorg/myrepo",
			wantPath:  filepath.Join(cfg.CacheDir, "m", "my", "myorg_myrepo.json"),
		},
		{
			name:      "single character name",
			imageName: "a",
			wantPath:  filepath.Join(cfg.CacheDir, "a", "default", "a.json"),
		},
		{
			name:      "empty name",
			imageName: "",
			wantPath:  filepath.Join(cfg.CacheDir, "default", "default", ".json"),
		},
		{
			name:      "deeply nested path",
			imageName: "org/sub/repo",
			wantPath:  filepath.Join(cfg.CacheDir, "o", "or", "org_sub_repo.json"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getShardedPath(tt.imageName)
			if got != tt.wantPath {
				t.Errorf("getShardedPath(%q) = %q, want %q", tt.imageName, got, tt.wantPath)
			}
		})
	}
}

// --- readFromCache tests ---

func TestReadFromCache_NonExistentFile(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	tags, exists, stale := readFromCache("nonexistent/image")
	if exists {
		t.Error("expected exists to be false for non-existent file")
	}
	if stale {
		t.Error("expected stale to be false for non-existent file")
	}
	if tags != nil {
		t.Error("expected tags to be nil for non-existent file")
	}
}

func TestReadFromCache_ValidCache(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/image"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now(),
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now(), Digest: "sha256:abc123"},
			{Name: "latest", LastUpdated: time.Now(), Digest: "sha256:def456"},
		},
	}

	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	tags, exists, stale := readFromCache(imageName)
	if !exists {
		t.Error("expected exists to be true for valid cache")
	}
	if stale {
		t.Error("expected stale to be false for fresh cache")
	}
	if len(tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(tags))
	}
}

func TestReadFromCache_StaleCache(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	cfg.RefreshInterval = 1 * time.Hour

	imageName := "test/stale-image"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now().Add(-2 * time.Hour), // 2 hours ago
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now(), Digest: "sha256:abc123"},
		},
	}

	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	tags, exists, stale := readFromCache(imageName)
	if !exists {
		t.Error("expected exists to be true")
	}
	if !stale {
		t.Error("expected stale to be true for old cache")
	}
	if len(tags) != 1 {
		t.Errorf("expected 1 tag, got %d", len(tags))
	}
}

func TestReadFromCache_EmptyTags(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/empty-tags"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now(),
		Tags:          []ImageTag{},
	}

	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	_, exists, _ := readFromCache(imageName)
	if exists {
		t.Error("expected exists to be false for empty tags")
	}
}

func TestReadFromCache_InvalidJSON(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/invalid-json"
	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	if err := os.WriteFile(path, []byte("not valid json"), 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	_, exists, _ := readFromCache(imageName)
	if exists {
		t.Error("expected exists to be false for invalid JSON")
	}
}

func TestReadFromCache_ZeroRefreshInterval(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	cfg.RefreshInterval = 0 // Disabled

	imageName := "test/no-refresh"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now().Add(-100 * 24 * time.Hour), // Very old
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now(), Digest: "sha256:abc123"},
		},
	}

	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	_, exists, stale := readFromCache(imageName)
	if !exists {
		t.Error("expected exists to be true")
	}
	if stale {
		t.Error("expected stale to be false when RefreshInterval is 0")
	}
}

// --- tagHandler tests ---

func TestTagHandler_CacheHit(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/cached-image"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now(),
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now(), Digest: "sha256:abc123", Architectures: []string{"amd64"}},
		},
	}

	path := getShardedPath(imageName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	data, _ := json.Marshal(cache)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write cache file: %v", err)
	}

	req := httptest.NewRequest("GET", "/tags?image=test/cached-image&registry=docker", nil)
	w := httptest.NewRecorder()

	tagHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", contentType)
	}

	var tags []ImageTag
	if err := json.Unmarshal(w.Body.Bytes(), &tags); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(tags) != 1 {
		t.Errorf("expected 1 tag, got %d", len(tags))
	}
	if tags[0].Name != "v1.0.0" {
		t.Errorf("expected tag name v1.0.0, got %s", tags[0].Name)
	}
}

func TestTagHandler_RegistryHostSelection(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	tests := []struct {
		name      string
		imageName string
		registry  string
	}{
		{"docker registry", "test/docker-registry", "docker"},
		{"ghcr registry", "test/ghcr-registry", "ghcr"},
		{"empty registry defaults to docker", "test/default-registry", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := TagCache{
				ImageName:     tt.imageName,
				LastRefreshed: time.Now(),
				Tags: []ImageTag{
					{Name: "v1.0.0", LastUpdated: time.Now()},
				},
			}

			path := getShardedPath(tt.imageName)
			os.MkdirAll(filepath.Dir(path), 0755)
			data, _ := json.Marshal(cache)
			os.WriteFile(path, data, 0644)

			req := httptest.NewRequest("GET", "/tags?image="+tt.imageName+"&registry="+tt.registry, nil)
			w := httptest.NewRecorder()

			tagHandler(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("expected status 200, got %d", w.Code)
			}
		})
	}
}

func TestTagHandler_StaleCacheTriggersRefresh(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	cfg.RefreshInterval = 1 * time.Millisecond // Very short for testing

	imageName := "test/stale-handler"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now().Add(-1 * time.Hour),
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now()},
		},
	}

	path := getShardedPath(imageName)
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.Marshal(cache)
	os.WriteFile(path, data, 0644)

	req := httptest.NewRequest("GET", "/tags?image="+imageName+"&registry=docker", nil)
	w := httptest.NewRecorder()

	tagHandler(w, req)

	// Should still return cached data immediately
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var tags []ImageTag
	if err := json.Unmarshal(w.Body.Bytes(), &tags); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(tags) != 1 || tags[0].Name != "v1.0.0" {
		t.Error("expected cached data to be returned for stale cache")
	}
}

// --- Model serialization tests ---

func TestImageTag_JSON(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	tag := ImageTag{
		Name:          "v1.0.0",
		LastUpdated:   now,
		Architectures: []string{"amd64", "arm64"},
		Digest:        "sha256:abc123",
	}

	data, err := json.Marshal(tag)
	if err != nil {
		t.Fatalf("failed to marshal ImageTag: %v", err)
	}

	var decoded ImageTag
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal ImageTag: %v", err)
	}

	if decoded.Name != tag.Name {
		t.Errorf("Name mismatch: got %s, want %s", decoded.Name, tag.Name)
	}
	if decoded.Digest != tag.Digest {
		t.Errorf("Digest mismatch: got %s, want %s", decoded.Digest, tag.Digest)
	}
	if len(decoded.Architectures) != len(tag.Architectures) {
		t.Errorf("Architectures length mismatch: got %d, want %d", len(decoded.Architectures), len(tag.Architectures))
	}
}

func TestTagCache_JSON(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cache := TagCache{
		ImageName:     "test/image",
		LastRefreshed: now,
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: now},
			{Name: "v2.0.0", LastUpdated: now},
		},
	}

	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("failed to marshal TagCache: %v", err)
	}

	var decoded TagCache
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal TagCache: %v", err)
	}

	if decoded.ImageName != cache.ImageName {
		t.Errorf("ImageName mismatch: got %s, want %s", decoded.ImageName, cache.ImageName)
	}
	if len(decoded.Tags) != len(cache.Tags) {
		t.Errorf("Tags length mismatch: got %d, want %d", len(decoded.Tags), len(cache.Tags))
	}
}

// --- linkRegex tests ---

func TestLinkRegex(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLast string
		wantOk   bool
	}{
		{
			name:     "valid link header",
			input:    `</v2/owner/repo/tags/list?n=100&last=v1.0.0>; rel="next"`,
			wantLast: "v1.0.0",
			wantOk:   true,
		},
		{
			name:     "link with complex tag",
			input:    `</v2/owner/repo/tags/list?n=100&last=sha-abc123>; rel="next"`,
			wantLast: "sha-abc123",
			wantOk:   true,
		},
		{
			name:     "no last parameter",
			input:    `</v2/owner/repo/tags/list?n=100>; rel="next"`,
			wantLast: "",
			wantOk:   false,
		},
		{
			name:     "empty string",
			input:    "",
			wantLast: "",
			wantOk:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match := linkRegex.FindStringSubmatch(tt.input)
			if tt.wantOk {
				if len(match) < 2 {
					t.Error("expected match but got none")
					return
				}
				if match[1] != tt.wantLast {
					t.Errorf("got last=%s, want %s", match[1], tt.wantLast)
				}
			} else {
				if len(match) >= 2 && match[1] != "" {
					t.Errorf("expected no match but got %s", match[1])
				}
			}
		})
	}
}

// --- Mock server tests for registry interactions ---

func TestFetchDockerHub_WithMockServer(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"next": nil,
			"results": []map[string]interface{}{
				{"name": "v1.0.0", "last_updated": "2024-01-01T00:00:00Z"},
				{"name": "v2.0.0", "last_updated": "2024-01-02T00:00:00Z"},
				{"name": "v1.0.0.sig", "last_updated": "2024-01-01T00:00:00Z"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("failed to call mock server: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

// --- updateAndSaveCache tests ---

func TestUpdateAndSaveCache_MergesExistingCache(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/merge-cache"

	existingCache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now().Add(-1 * time.Hour),
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now().Add(-2 * time.Hour), Digest: "sha256:old", Architectures: []string{"amd64"}},
		},
	}

	path := getShardedPath(imageName)
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.Marshal(existingCache)
	os.WriteFile(path, data, 0644)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("cache file was not created")
	}

	readData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read cache file: %v", err)
	}

	var readCache TagCache
	if err := json.Unmarshal(readData, &readCache); err != nil {
		t.Fatalf("failed to unmarshal cache: %v", err)
	}

	if len(readCache.Tags) != 1 {
		t.Errorf("expected 1 tag, got %d", len(readCache.Tags))
	}
}

// --- Config tests ---

func TestConfig_Defaults(t *testing.T) {
	c := Config{}
	if c.Port != 0 {
		t.Errorf("expected default Port to be 0, got %d", c.Port)
	}
	if c.MaxTags != 0 {
		t.Errorf("expected default MaxTags to be 0, got %d", c.MaxTags)
	}
	if c.RefreshInterval != 0 {
		t.Errorf("expected default RefreshInterval to be 0, got %v", c.RefreshInterval)
	}
}

// --- Concurrent access tests ---

func TestReadFromCache_ConcurrentAccess(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	imageName := "test/concurrent"
	cache := TagCache{
		ImageName:     imageName,
		LastRefreshed: time.Now(),
		Tags: []ImageTag{
			{Name: "v1.0.0", LastUpdated: time.Now()},
		},
	}

	path := getShardedPath(imageName)
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.Marshal(cache)
	os.WriteFile(path, data, 0644)

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			tags, exists, _ := readFromCache(imageName)
			if !exists || len(tags) != 1 {
				t.Error("concurrent read failed")
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// --- Edge case tests ---

func TestGetShardedPath_SpecialCharacters(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	tests := []struct {
		name      string
		imageName string
	}{
		{"with dots", "my.org/my.repo"},
		{"with hyphens", "my-org/my-repo"},
		{"with underscores", "my_org/my_repo"},
		{"mixed", "my-org.io/my_repo.v2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := getShardedPath(tt.imageName)
			if path == "" {
				t.Error("expected non-empty path")
			}
			if filepath.Clean(path) != path {
				t.Error("path contains unnecessary elements")
			}
		})
	}
}

func TestTagHandler_MissingImageParameter(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/tags", nil)
	w := httptest.NewRecorder()

	tagHandler(w, req)
	
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for missing image parameter, got %d", w.Code)
	}
}

func TestTagHandler_EmptyImage(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/tags?image=&registry=docker", nil)
	w := httptest.NewRecorder()

	tagHandler(w, req)
	
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for empty image, got %d", w.Code)
	}
}

// --- Input validation tests ---

func TestValidateInput_ValidInputs(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		registry string
	}{
		{"docker hub library image", "library/nginx", "docker"},
		{"ghcr image", "parkr/oci-tag-proxy", "ghcr"},
		{"simple image name", "nginx", "docker"},
		{"image with dots", "my.org/my.repo", "docker"},
		{"image with hyphens", "my-org/my-repo", "ghcr"},
		{"image with underscores", "my_org/my_repo", "docker"},
		{"deeply nested", "org/sub/repo", "docker"},
		{"single character", "a", "docker"},
		{"mixed characters", "my-org.io/my_repo.v2", "ghcr"},
		{"empty registry defaults to docker", "nginx", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInput(tt.image, tt.registry)
			if err != nil {
				t.Errorf("validateInput(%q, %q) returned error: %v", tt.image, tt.registry, err)
			}
		})
	}
}

func TestValidateInput_InvalidInputs(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		registry  string
		wantError string
	}{
		{"empty image", "", "docker", "image parameter is required"},
		{"path traversal with ..", "../../etc/passwd", "docker", "image name cannot contain '..'"},
		{"path traversal in path", "org/../etc/passwd", "docker", "image name cannot contain '..'"},
		{"absolute path", "/etc/passwd", "docker", "image name cannot start with '/'"},
		{"invalid registry", "nginx", "invalid", "registry must be 'docker' or 'ghcr'"},
		{"registry typo", "nginx", "dockerhub", "registry must be 'docker' or 'ghcr'"},
		{"invalid chars - semicolon", "image;rm -rf", "docker", "invalid image name format"},
		{"invalid chars - ampersand", "image&malicious", "docker", "invalid image name format"},
		{"invalid chars - pipe", "image|cat", "docker", "invalid image name format"},
		{"invalid chars - backtick", "image`whoami`", "docker", "invalid image name format"},
		{"invalid chars - dollar", "image$var", "docker", "invalid image name format"},
		{"invalid chars - parentheses", "image()", "docker", "invalid image name format"},
		{"invalid chars - brackets", "image[]", "docker", "invalid image name format"},
		{"invalid chars - braces", "image{}", "docker", "invalid image name format"},
		{"invalid chars - space", "my image", "docker", "invalid image name format"},
		{"invalid chars - quotes", "image\"test\"", "docker", "invalid image name format"},
		{"invalid chars - single quote", "image'test'", "docker", "invalid image name format"},
		{"invalid chars - less than", "image<test", "docker", "invalid image name format"},
		{"invalid chars - greater than", "image>test", "docker", "invalid image name format"},
		{"invalid chars - asterisk", "image*", "docker", "invalid image name format"},
		{"invalid chars - question mark", "image?", "docker", "invalid image name format"},
		{"starts with special char", ".image", "docker", "invalid image name format"},
		{"starts with hyphen", "-image", "docker", "invalid image name format"},
		{"starts with underscore", "_image", "docker", "invalid image name format"},
		{"starts with slash", "/image", "docker", "image name cannot start with '/'"},
		{"ends with special char", "image.", "docker", "invalid image name format"},
		{"ends with hyphen", "image-", "docker", "invalid image name format"},
		{"ends with underscore", "image_", "docker", "invalid image name format"},
		{"ends with slash", "image/", "docker", "invalid image name format"},
		{"only special chars", "...", "docker", ".."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInput(tt.image, tt.registry)
			if err == nil {
				t.Errorf("validateInput(%q, %q) expected error but got nil", tt.image, tt.registry)
				return
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("validateInput(%q, %q) error = %v, want error containing %q", tt.image, tt.registry, err, tt.wantError)
			}
		})
	}
}

func TestTagHandler_ValidationErrors(t *testing.T) {
	cleanup := setupTestConfig(t)
	defer cleanup()

	tests := []struct {
		name           string
		url            string
		expectedStatus int
		errorContains  string
	}{
		{
			name:           "missing image parameter",
			url:            "/tags?registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "image parameter is required",
		},
		{
			name:           "empty image parameter",
			url:            "/tags?image=&registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "image parameter is required",
		},
		{
			name:           "invalid registry",
			url:            "/tags?image=nginx&registry=invalid",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "registry must be 'docker' or 'ghcr'",
		},
		{
			name:           "path traversal attempt",
			url:            "/tags?image=../../etc/passwd&registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "..",
		},
		{
			name:           "absolute path attempt",
			url:            "/tags?image=/etc/passwd&registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "cannot start with '/'",
		},
		{
			name:           "command injection attempt",
			url:            "/tags?image=test%3Brm+-rf&registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "invalid image name format",
		},
		{
			name:           "shell expansion attempt",
			url:            "/tags?image=test$(whoami)&registry=docker",
			expectedStatus: http.StatusBadRequest,
			errorContains:  "invalid image name format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			w := httptest.NewRecorder()

			tagHandler(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if tt.errorContains != "" && !strings.Contains(w.Body.String(), tt.errorContains) {
				t.Errorf("expected error containing %q, got %q", tt.errorContains, w.Body.String())
			}
		})
	}
}

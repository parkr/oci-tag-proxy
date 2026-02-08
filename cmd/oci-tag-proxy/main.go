package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/singleflight"
)

// --- Prometheus Metrics ---
var (
	opsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "tag_proxy_requests_total", Help: "Total tag requests."},
		[]string{"registry", "status"},
	)
	fetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "tag_proxy_registry_fetch_seconds", Help: "Registry fetch latency.", Buckets: prometheus.DefBuckets},
		[]string{"registry"},
	)
)

// --- Models ---
type Config struct {
	Port            int
	CacheDir        string
	MaxTags         int
	DockerHubJWT    string
	GitHubToken     string
	RefreshInterval time.Duration
}

type ImageTag struct {
	Name          string    `json:"name"`
	LastUpdated   time.Time `json:"last_updated"`
	Architectures []string  `json:"architectures"`
	Digest        string    `json:"digest"`
}

type TagCache struct {
	ImageName     string     `json:"image_name"`
	Tags          []ImageTag `json:"tags"`
	LastRefreshed time.Time  `json:"last_refreshed"`
}

var (
	cfg          Config
	requestGroup singleflight.Group
	fileMu       sync.Map
	linkRegex    = regexp.MustCompile(`last=([^&>]+)`)
	// imageRegex validates image names: alphanumeric, hyphens, underscores, dots, and slashes
	imageRegex   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*[a-zA-Z0-9]$|^[a-zA-Z0-9]$`)
	httpClient   *retryablehttp.Client
)

func init() {
	prometheus.MustRegister(opsCounter, fetchDuration)
}

// --- Storage Logic ---

func getShardedPath(imageName string) (string, error) {
	// Validate that imageName is not empty
	if imageName == "" {
		return "", fmt.Errorf("image name cannot be empty")
	}

	// Replace any path separators in the image name so that user input cannot
	// introduce additional path components.
	safeName := strings.ReplaceAll(imageName, "/", "_")

	// Derive sharding directories from the sanitized name. These are always
	// single characters/slices, so they cannot contain path separators.
	p1, p2 := "default", "default"
	if len(safeName) > 0 {
		p1 = string(safeName[0])
	}
	if len(safeName) > 1 {
		p2 = safeName[0:2]
	}

	// Ensure the cache directory itself is an absolute, normalized path.
	// An empty or "." cache directory is resolved to the current working
	// directory so that all cache files remain beneath a well-defined base.
	baseDir := cfg.CacheDir
	if baseDir == "" || baseDir == "." {
		if cwd, err := os.Getwd(); err == nil {
			baseDir = cwd
		} else {
			baseDir = "."
		}
	}
	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		// In the unlikely event of an error resolving the cache directory,
		// fall back to a local relative directory.
		absBase = "."
	}

	// Construct the final file name and then take its base component to
	// guarantee that the result is a single path element, even if
	// safeName contained unexpected characters in the future.
	cacheFile := filepath.Base(safeName + ".json")

	return filepath.Join(absBase, p1, p2, cacheFile), nil
}

// readFromCache returns cached tags, whether cache exists, and whether it's stale
func readFromCache(imageName string) (tags []ImageTag, exists bool, stale bool) {
	path, err := getShardedPath(imageName)
	if err != nil {
		return nil, false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, false
	}
	var cache TagCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, false, false
	}
	if len(cache.Tags) == 0 {
		return nil, false, false
	}
	isStale := cfg.RefreshInterval > 0 && time.Since(cache.LastRefreshed) > cfg.RefreshInterval
	return cache.Tags, true, isStale
}

// --- Registry & Manifest Logic ---

// fetchDockerHubJWT fetches a JWT from Docker Hub's login API
func fetchDockerHubJWT() (string, error) {
	username := os.Getenv("DOCKER_HUB_USERNAME")
	password := os.Getenv("DOCKER_HUB_PASSWORD")

	if username == "" || password == "" {
		log.Println("Docker Hub username or password not set; proceeding unauthenticated.")
		return "", nil
	}

	type loginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	type loginResponse struct {
		Token string `json:"token"`
	}

	reqBody, err := json.Marshal(loginRequest{
		Username: username,
		Password: password,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal login request: %w", err)
	}

	req, err := retryablehttp.NewRequest("POST", "https://hub.docker.com/v2/users/login/", strings.NewReader(string(reqBody)))
	if err != nil {
		return "", fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to login to Docker Hub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Docker Hub login failed with status %d", resp.StatusCode)
	}

	var loginResp loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return "", fmt.Errorf("failed to decode login response: %w", err)
	}

	log.Println("Successfully authenticated with Docker Hub")
	return loginResp.Token, nil
}

func getAuthToken(registry, image string) (string, error) {
	var url string
	if registry == "ghcr.io" {
		// If GITHUB_TOKEN is set, use it directly for GHCR authentication
		if cfg.GitHubToken != "" {
			return cfg.GitHubToken, nil
		}
		url = fmt.Sprintf("https://ghcr.io/token?scope=repository:%s:pull&service=ghcr.io", image)
	} else {
		url = fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", image)
	}
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var auth struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&auth)
	return auth.Token, nil
}

func getManifestMetadata(registry, token, image, tag, cachedDigest string) (string, []string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, image, tag)

	req, _ := http.NewRequest("HEAD", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")

	retryableReq, err := retryablehttp.FromRequest(req)
	if err != nil {
		return "", nil, err
	}
	resp, err := httpClient.Do(retryableReq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	newDigest := resp.Header.Get("Docker-Content-Digest")
	if newDigest != "" && newDigest == cachedDigest {
		return newDigest, nil, nil // Digest matches cache
	}

	// Fetch full manifest if digest changed
	req.Method = "GET"
	retryableReqGET, err := retryablehttp.FromRequest(req)
	if err != nil {
		return newDigest, nil, err
	}
	respGET, err := httpClient.Do(retryableReqGET)
	if err != nil {
		return newDigest, nil, err
	}
	defer respGET.Body.Close()

	var data struct {
		Manifests []struct {
			Platform struct {
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	json.NewDecoder(respGET.Body).Decode(&data)

	var archs []string
	for _, m := range data.Manifests {
		if m.Platform.Architecture != "" {
			archs = append(archs, m.Platform.Architecture)
		}
	}
	return newDigest, archs, nil
}

// --- Fetchers ---

func fetchDockerHub(image string) ([]ImageTag, error) {
	timer := prometheus.NewTimer(fetchDuration.WithLabelValues("docker"))
	defer timer.ObserveDuration()

	var tags []ImageTag
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags/?page_size=100", image)
	for url != "" {
		req, _ := retryablehttp.NewRequest("GET", url, nil)
		if cfg.DockerHubJWT != "" {
			req.Header.Set("Authorization", "JWT "+cfg.DockerHubJWT)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var data struct {
			Next    *string `json:"next"`
			Results []struct {
				Name        string    `json:"name"`
				LastUpdated time.Time `json:"last_updated"`
			} `json:"results"`
		}
		json.NewDecoder(resp.Body).Decode(&data)
		for _, r := range data.Results {
			if !strings.HasSuffix(r.Name, ".sig") {
				tags = append(tags, ImageTag{Name: r.Name, LastUpdated: r.LastUpdated})
			}
		}
		url = ""
		if data.Next != nil {
			url = *data.Next
		}
	}
	return tags, nil
}

func fetchGHCR(image string) ([]ImageTag, error) {
	timer := prometheus.NewTimer(fetchDuration.WithLabelValues("ghcr"))
	defer timer.ObserveDuration()

	token, err := getAuthToken("ghcr.io", image)
	if err != nil {
		return nil, err
	}

	var tags []ImageTag
	url := fmt.Sprintf("https://ghcr.io/v2/%s/tags/list?n=100", image)
	now := time.Now()
	for url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		retryableReq, err := retryablehttp.FromRequest(req)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(retryableReq)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var data struct {
			Tags []string `json:"tags"`
		}
		json.NewDecoder(resp.Body).Decode(&data)
		for _, t := range data.Tags {
			if !strings.HasSuffix(t, ".sig") {
				tags = append(tags, ImageTag{Name: t, LastUpdated: now})
			}
		}
		link := resp.Header.Get("Link")
		url = ""
		if strings.Contains(link, `rel="next"`) {
			if match := linkRegex.FindStringSubmatch(link); len(match) > 1 {
				url = fmt.Sprintf("https://ghcr.io/v2/%s/tags/list?n=100&last=%s", image, match[1])
			}
		}
	}
	return tags, nil
}

// --- Orchestration ---

func updateAndSaveCache(imageName, registryHost string, incoming []ImageTag) ([]ImageTag, error) {
	mu, _ := fileMu.LoadOrStore(imageName, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	path, err := getShardedPath(imageName)
	if err != nil {
		return nil, err
	}
	os.MkdirAll(filepath.Dir(path), 0755)

	cacheMap := make(map[string]ImageTag)
	if data, err := os.ReadFile(path); err == nil {
		var cache TagCache
		if err := json.Unmarshal(data, &cache); err != nil {
			log.Printf("Error unmarshaling cache for image=%s: %v", imageName, err)
		}
		for _, t := range cache.Tags {
			cacheMap[t.Name] = t
		}
	}

	token, err := getAuthToken(registryHost, imageName)
	if err != nil {
		log.Printf("Error getting auth token for image=%s registry=%s: %v", imageName, registryHost, err)
	}
	
	for i, it := range incoming {
		existing := cacheMap[it.Name]
		digest, archs, err := getManifestMetadata(registryHost, token, imageName, it.Name, existing.Digest)
		if err != nil {
			log.Printf("Error getting manifest metadata for image=%s tag=%s: %v", imageName, it.Name, err)
			continue
		}

		incoming[i].Digest = digest
		if archs != nil {
			incoming[i].Architectures = archs
		} else {
			incoming[i].Architectures = existing.Architectures
		}
		cacheMap[it.Name] = incoming[i]
	}

	var merged []ImageTag
	for _, v := range cacheMap {
		merged = append(merged, v)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].LastUpdated.After(merged[j].LastUpdated) })
	if len(merged) > cfg.MaxTags {
		merged = merged[:cfg.MaxTags]
	}

	output, err := json.MarshalIndent(TagCache{ImageName: imageName, Tags: merged, LastRefreshed: time.Now()}, "", "  ")
	if err != nil {
		log.Printf("Error marshaling cache for image=%s: %v", imageName, err)
		return merged, err
	}
	
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, output, 0644); err != nil {
		log.Printf("Error writing cache file for image=%s: %v", imageName, err)
		return merged, err
	}
	
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("Error renaming cache file for image=%s: %v", imageName, err)
		return merged, err
	}
	
	return merged, nil
}

func refreshCache(image, regType, host string) {
	_, _, _ = requestGroup.Do(image, func() (interface{}, error) {
		var tags []ImageTag
		var err error
		if regType == "ghcr" {
			tags, err = fetchGHCR(image)
		} else {
			tags, err = fetchDockerHub(image)
		}
		if err != nil {
			log.Printf("Background refresh failed for %s: %v", image, err)
			return nil, err
		}
		_, err = updateAndSaveCache(image, host, tags)
		if err != nil {
			log.Printf("Background cache update failed for %s: %v", image, err)
		}
		return nil, err
	})
}

// validateInput validates the image and registry parameters from the request
func validateInput(image, registry string) error {
	// Validate image parameter
	if image == "" {
		return fmt.Errorf("image parameter is required")
	}

	// Ensure image name doesn't start with / (absolute path)
	if strings.HasPrefix(image, "/") {
		return fmt.Errorf("image name cannot start with '/'")
	}

	// Prevent path traversal attacks
	if strings.Contains(image, "..") {
		return fmt.Errorf("image name cannot contain '..'")
	}

	// Validate image format: must match allowed pattern
	if !imageRegex.MatchString(image) {
		return fmt.Errorf("invalid image name format")
	}

	// Validate registry parameter
	if registry != "" && registry != "docker" && registry != "ghcr" {
		return fmt.Errorf("registry must be 'docker' or 'ghcr'")
	}

	return nil
}

func tagHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	image := r.URL.Query().Get("image")
	regType := r.URL.Query().Get("registry")

	// Validate input parameters
	if err := validateInput(image, regType); err != nil {
		log.Printf("Request failed: image=%s registry=%s error=%v latency=%v", image, regType, err, time.Since(startTime))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	host := "registry-1.docker.io"
	if regType == "ghcr" {
		host = "ghcr.io"
	}

	// Check cache first - return immediately if cached data exists
	if cached, exists, stale := readFromCache(image); exists {
		if stale {
			// Trigger background refresh for stale cache
			go refreshCache(image, regType, host)
			opsCounter.WithLabelValues(regType, "cache_stale").Inc()
		} else {
			opsCounter.WithLabelValues(regType, "cache_hit").Inc()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cached)
		log.Printf("Request completed: image=%s registry=%s tags=%d cache_hit=true cache_stale=%v latency=%v", 
			image, regType, len(cached), stale, time.Since(startTime))
		return
	}

	// Cache miss - fetch from registry
	res, err, _ := requestGroup.Do(image, func() (interface{}, error) {
		var tags []ImageTag
		var err error
		if regType == "ghcr" {
			tags, err = fetchGHCR(image)
		} else {
			tags, err = fetchDockerHub(image)
		}
		if err != nil {
			log.Printf("Error fetching from upstream: image=%s registry=%s error=%v", image, regType, err)
			return nil, err
		}
		return updateAndSaveCache(image, host, tags)
	})

	if err != nil {
		opsCounter.WithLabelValues(regType, "error").Inc()
		log.Printf("Request failed: image=%s registry=%s error=%v latency=%v", image, regType, err, time.Since(startTime))
		http.Error(w, err.Error(), 500)
		return
	}
	
	tags, ok := res.([]ImageTag)
	if !ok {
		opsCounter.WithLabelValues(regType, "error").Inc()
		log.Printf("Request failed: image=%s registry=%s error=invalid response type latency=%v", image, regType, time.Since(startTime))
		http.Error(w, "Internal error: invalid response type", 500)
		return
	}
	
	opsCounter.WithLabelValues(regType, "success").Inc()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tags)
	log.Printf("Request completed: image=%s registry=%s tags=%d cache_hit=false latency=%v", 
		image, regType, len(tags), time.Since(startTime))
}

func main() {
	var refreshIntervalStr string
	flag.IntVar(&cfg.Port, "port", 8080, "Listen port")
	flag.StringVar(&cfg.CacheDir, "dir", "./tag_cache", "Cache directory")
	flag.IntVar(&cfg.MaxTags, "max-tags", 1000, "Max tags per image")
	flag.StringVar(&refreshIntervalStr, "refresh-interval", "24h", "Cache refresh interval (e.g., 1h, 24h, 7d)")
	flag.Parse()

	var err error
	cfg.RefreshInterval, err = time.ParseDuration(refreshIntervalStr)
	if err != nil {
		log.Fatalf("Invalid refresh interval: %v", err)
	}

	// Initialize HTTP client before fetching tokens
	httpClient = retryablehttp.NewClient()
	httpClient.RetryMax = 3
	httpClient.Logger = nil

	// Read authentication tokens from environment variables
	cfg.GitHubToken = os.Getenv("GITHUB_TOKEN")
	if cfg.GitHubToken != "" {
		log.Println("Using GITHUB_TOKEN for GHCR authentication")
	}

	// Fetch Docker Hub JWT if credentials are available
	cfg.DockerHubJWT, err = fetchDockerHubJWT()
	if err != nil {
		log.Printf("Warning: Failed to fetch Docker Hub JWT: %v", err)
	}

	if err = os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		log.Fatalf("Failed to create cache directory: %v", err)
	}
	http.HandleFunc("/tags", tagHandler)
	http.Handle("/metrics", promhttp.Handler())

	log.Printf("Starting proxy on :%d", cfg.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", cfg.Port), nil))
}

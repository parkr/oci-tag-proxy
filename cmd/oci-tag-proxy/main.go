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
	Port         int
	CacheDir     string
	MaxTags      int
	DockerHubJWT string
}

type ImageTag struct {
	Name          string    `json:"name"`
	LastUpdated   time.Time `json:"last_updated"`
	Architectures []string  `json:"architectures"`
	Digest        string    `json:"digest"`
}

type TagCache struct {
	ImageName string     `json:"image_name"`
	Tags      []ImageTag `json:"tags"`
}

var (
	cfg          Config
	requestGroup singleflight.Group
	fileMu       sync.Map
	linkRegex    = regexp.MustCompile(`last=([^&>]+)`)
	httpClient   *retryablehttp.Client
)

func init() {
	prometheus.MustRegister(opsCounter, fetchDuration)
}

// --- Storage Logic ---

func getShardedPath(imageName string) string {
	safeName := strings.ReplaceAll(imageName, "/", "_")
	p1, p2 := "default", "default"
	if len(safeName) > 0 { p1 = string(safeName[0]) }
	if len(safeName) > 1 { p2 = safeName[0:2] }
	return filepath.Join(cfg.CacheDir, p1, p2, safeName+".json")
}

// --- Registry & Manifest Logic ---

func getAuthToken(registry, image string) (string, error) {
	var url string
	if registry == "ghcr.io" {
		url = fmt.Sprintf("https://ghcr.io/token?scope=repository:%s:pull&service=ghcr.io", image)
	} else {
		url = fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", image)
	}
	resp, err := httpClient.Get(url)
	if err != nil { return "", err }
	defer resp.Body.Close()
	var auth struct{ Token string `json:"token"` }
	json.NewDecoder(resp.Body).Decode(&auth)
	return auth.Token, nil
}

func getManifestMetadata(registry, token, image, tag, cachedDigest string) (string, []string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, image, tag)
	
	req, _ := http.NewRequest("HEAD", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")

	retryableReq, err := retryablehttp.FromRequest(req)
	if err != nil { return "", nil, err }
	resp, err := httpClient.Do(retryableReq)
	if err != nil { return "", nil, err }
	defer resp.Body.Close()

	newDigest := resp.Header.Get("Docker-Content-Digest")
	if newDigest != "" && newDigest == cachedDigest {
		return newDigest, nil, nil // Digest matches cache
	}

	// Fetch full manifest if digest changed
	req.Method = "GET"
	retryableReqGET, err := retryablehttp.FromRequest(req)
	if err != nil { return newDigest, nil, err }
	respGET, err := httpClient.Do(retryableReqGET)
	if err != nil { return newDigest, nil, err }
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
		if m.Platform.Architecture != "" { archs = append(archs, m.Platform.Architecture) }
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
		if cfg.DockerHubJWT != "" { req.Header.Set("Authorization", "JWT "+cfg.DockerHubJWT) }
		resp, err := httpClient.Do(req)
		if err != nil { return nil, err }
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
		if data.Next != nil { url = *data.Next }
	}
	return tags, nil
}

func fetchGHCR(image string) ([]ImageTag, error) {
	timer := prometheus.NewTimer(fetchDuration.WithLabelValues("ghcr"))
	defer timer.ObserveDuration()

	token, err := getAuthToken("ghcr.io", image)
	if err != nil { return nil, err }

	var tags []ImageTag
	url := fmt.Sprintf("https://ghcr.io/v2/%s/tags/list?n=100", image)
	now := time.Now()
	for url != "" {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		retryableReq, err := retryablehttp.FromRequest(req)
		if err != nil { return nil, err }
		resp, err := httpClient.Do(retryableReq)
		if err != nil { return nil, err }
		defer resp.Body.Close()

		var data struct{ Tags []string `json:"tags"` }
		json.NewDecoder(resp.Body).Decode(&data)
		for _, t := range data.Tags {
			if !strings.HasSuffix(t, ".sig") { tags = append(tags, ImageTag{Name: t, LastUpdated: now}) }
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

	path := getShardedPath(imageName)
	os.MkdirAll(filepath.Dir(path), 0755)

	cacheMap := make(map[string]ImageTag)
	if data, err := os.ReadFile(path); err == nil {
		var cache TagCache
		json.Unmarshal(data, &cache)
		for _, t := range cache.Tags { cacheMap[t.Name] = t }
	}

	token, _ := getAuthToken(registryHost, imageName)
	for i, it := range incoming {
		existing := cacheMap[it.Name]
		digest, archs, err := getManifestMetadata(registryHost, token, imageName, it.Name, existing.Digest)
		if err != nil { continue }

		incoming[i].Digest = digest
		if archs != nil { incoming[i].Architectures = archs } else { incoming[i].Architectures = existing.Architectures }
		cacheMap[it.Name] = incoming[i]
	}

	var merged []ImageTag
	for _, v := range cacheMap { merged = append(merged, v) }
	sort.Slice(merged, func(i, j int) bool { return merged[i].LastUpdated.After(merged[j].LastUpdated) })
	if len(merged) > cfg.MaxTags { merged = merged[:cfg.MaxTags] }

	output, _ := json.MarshalIndent(TagCache{ImageName: imageName, Tags: merged}, "", "  ")
	tmp := path + ".tmp"
	os.WriteFile(tmp, output, 0644)
	return merged, os.Rename(tmp, path)
}

func tagHandler(w http.ResponseWriter, r *http.Request) {
	image := r.URL.Query().Get("image")
	regType := r.URL.Query().Get("registry")
	host := "registry-1.docker.io"
	if regType == "ghcr" { host = "ghcr.io" }

	res, err, _ := requestGroup.Do(image, func() (interface{}, error) {
		var tags []ImageTag
		var err error
		if regType == "ghcr" { tags, err = fetchGHCR(image) } else { tags, err = fetchDockerHub(image) }
		if err != nil { return nil, err }
		return updateAndSaveCache(image, host, tags)
	})

	if err != nil {
		opsCounter.WithLabelValues(regType, "error").Inc()
		http.Error(w, err.Error(), 500)
		return
	}
	opsCounter.WithLabelValues(regType, "success").Inc()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func main() {
	flag.IntVar(&cfg.Port, "port", 8080, "Listen port")
	flag.StringVar(&cfg.CacheDir, "dir", "./tag_cache", "Cache directory")
	flag.IntVar(&cfg.MaxTags, "max-tags", 1000, "Max tags per image")
	flag.StringVar(&cfg.DockerHubJWT, "jwt", "", "Docker Hub JWT")
	flag.Parse()

	httpClient = retryablehttp.NewClient()
	httpClient.RetryMax = 3
	httpClient.Logger = nil

	os.MkdirAll(cfg.CacheDir, 0755)
	http.HandleFunc("/tags", tagHandler)
	http.Handle("/metrics", promhttp.Handler())

	log.Printf("Starting proxy on :%d", cfg.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", cfg.Port), nil))
}

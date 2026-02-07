# oci-tag-proxy
A proxy for OCI repositories which allows me to query tags more effectively and frequently.

## Docker

Docker images are automatically built and published to GitHub Container Registry when code is pushed to the main branch.

```bash
# Pull the latest image
docker pull ghcr.io/parkr/oci-tag-proxy:latest

# Run the container
docker run -p 8080:8080 ghcr.io/parkr/oci-tag-proxy:latest
```

The Docker image is built using a multi-stage build process with a minimal distroless base for security and size efficiency (~15MB).

## API Usage

Once the server is running, you can query container image tags using the `/tags` endpoint.

### Endpoint: `/tags`

#### Required Parameters

- `image` - The image name to query (e.g., `library/nginx` for Docker Hub, `parkr/oci-tag-proxy` for GHCR)
- `registry` - The registry type, either `docker` for Docker Hub or `ghcr` for GitHub Container Registry

#### Example Requests

Query tags for a Docker Hub image:
```bash
curl "http://localhost:8080/tags?image=library/nginx&registry=docker"
```

Query tags for a GHCR image:
```bash
curl "http://localhost:8080/tags?image=parkr/oci-tag-proxy&registry=ghcr"
```

#### Response Format

The API returns a JSON array of tag objects. Each tag contains:

- `name` - The tag name (e.g., "latest", "1.0.0")
- `last_updated` - ISO 8601 timestamp of when the tag was last updated
- `architectures` - Array of supported architectures (e.g., ["amd64", "arm64"])
- `digest` - The manifest digest for the tag

Example response:
```json
[
  {
    "name": "latest",
    "last_updated": "2026-02-07T08:15:30Z",
    "architectures": ["amd64", "arm64"],
    "digest": "sha256:abc123..."
  },
  {
    "name": "1.0.0",
    "last_updated": "2026-02-05T10:20:15Z",
    "architectures": ["amd64"],
    "digest": "sha256:def456..."
  }
]
```

#### Error Handling

If an error occurs (e.g., invalid image name, registry unavailable, network error), the API returns:
- **HTTP Status**: 500 Internal Server Error
- **Response Body**: Plain text error message describing what went wrong

Example error response:
```
HTTP/1.1 500 Internal Server Error
Content-Type: text/plain

Get "https://hub.docker.com/v2/repositories/invalid/image/tags/": 404 Not Found
```

### Endpoint: `/metrics`

The server also exposes Prometheus metrics at the `/metrics` endpoint for monitoring:

```bash
curl "http://localhost:8080/metrics"
```

This endpoint provides metrics including:
- `tag_proxy_requests_total` - Total number of tag requests by registry and status
- `tag_proxy_registry_fetch_seconds` - Registry fetch latency histogram

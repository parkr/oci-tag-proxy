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

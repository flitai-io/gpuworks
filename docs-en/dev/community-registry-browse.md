# Community Registry Browsing

On the Images page, use **Community Images → Add Community Image** to browse tags from Docker Hub, Quay, and OCI Distribution registries. Browsing retrieves tag metadata only. It does not download image layers or automatically import every tag into the image catalog.

Administrators configure repository cards through `image_community_catalog`. Repositories containing multiple engines or hardware frameworks can omit `engine` and `framework`. Selecting a tag automatically fills the engine, engine version, framework, framework version, and CPU architecture explicitly declared in that tag. Tag metadata takes precedence over repository defaults, and users can still edit the values. Switching tags clears old versions that cannot be identified from the new tag; an unknown architecture resets to `multi`.

For example, `cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16` fills in SGLang `0.5.15.post1`, CANN `9.0`, and `arm64`. A standalone `omni` marker in a vLLM tag identifies the `vllm-omni` image family. Tags such as `latest`, which contain no version information, do not receive a guessed framework version.

For mixed repositories without uniform defaults, the browser loads tag metadata in the background, querying at most two repositories concurrently. It derives engine and framework badges from tags still present upstream, without downloading image layers or adding a persistent capability list. If a request fails, the card displays a failure message; selecting the repository retries the request. These form values and badges describe the tag declarations, not verified image contents or hardware compatibility.

Docker Hub uses the tag API at `hub.docker.com`, and Quay uses its public tag API. Both support public repository browsing; dedicated authentication for private Docker Hub or Quay APIs is outside the current scope.

OCI registry browsing reads the platform's existing host credential settings and uses the username and password for the corresponding registry. It supports direct Basic authentication and Bearer token exchanges with the same HTTPS host. Credentials are not included in repository cards, image catalog entries, or API responses.

Authentication is restricted to the registry's exact HTTPS host and port. Custom API addresses, token services on other hosts, pagination, and HTTP redirects cannot obtain the registry password or Bearer token outside that scope. Cross-origin pagination and authenticated cross-origin redirects fail. Private OCI registries that require an authentication service on another host need additional support.

Successful tag lists are cached for one hour. Changing or deleting login information invalidates private list caches associated with the previous credentials. Failed queries are not cached as successful lists. Newly uploaded tags may appear after the cache expires.

Browsing, registering, pulling, and running inference are separate steps. Selecting and adding a tag registers the image in the community catalog. Deployment still requires compatible hardware, architecture, drivers, models, and startup templates.

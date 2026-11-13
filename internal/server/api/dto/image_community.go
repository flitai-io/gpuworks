package dto

// CommunityRepo is one community repository: a card in the community tab
// (listed without tags) or the result of browsing one repository's tags.
type CommunityRepo struct {
	Repo string `json:"repo"`
	// Registry is the tag-list API: docker-hub, quay, or oci.
	Registry string `json:"registry"`
	// AccessHost is the first tag API's host, with no credentials, path or port.
	AccessHost string `json:"access_host,omitempty"`
	Engine     string `json:"engine"`
	Framework  string `json:"framework,omitempty"`
	// Siblings are further repositories on the same card; each is browsed
	// on its own and inherits the card's engine and framework.
	Siblings []string       `json:"siblings,omitempty"`
	Tags     []CommunityTag `json:"tags,omitempty"`
}

// CommunityTag is one upstream tag. Every tag the registry lists is
// included; rolling ones (latest, nightly, ...) are marked.
type CommunityTag struct {
	Tag              string `json:"tag"`
	Engine           string `json:"engine,omitempty"`
	EngineVersion    string `json:"engine_version,omitempty"`
	Framework        string `json:"framework,omitempty"`
	FrameworkVersion string `json:"framework_version,omitempty"`
	Arch             string `json:"arch,omitempty"`
	RollingTag       bool   `json:"rolling_tag,omitempty"`
	// UpstreamGone marks a tag listed before that the registry no longer has.
	UpstreamGone bool `json:"upstream_gone,omitempty"`
	// LastUpdated is the registry's timestamp for the tag, when it has one.
	LastUpdated string `json:"last_updated,omitempty"`
}

// ImageEngineOption is one engine value an image catalog entry may declare,
// with its product name, in the order the image form offers them.
type ImageEngineOption struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

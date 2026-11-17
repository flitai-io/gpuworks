package vast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Vast's v1 instance list has a maximum page size of 25.
// https://docs.vast.ai/api-reference/instances/show-instances
const instancePageSize = 25

type instancePage struct {
	Instances []instance
	NextToken string
	Total     int
}

// instanceListURL upgrades only the legacy /api/v0 suffix for instance queries.
// Other configured paths and versions are used as supplied.
func instanceListURL(base string, query url.Values) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/api/v0") {
		u.Path = strings.TrimSuffix(u.Path, "/api/v0") + "/api/v1"
	}
	u.Path += "/instances/"
	u.RawPath = ""
	params := u.Query()
	for key, values := range query {
		params[key] = values
	}
	u.RawQuery = params.Encode()
	return u.String(), nil
}

func (d *Driver) listInstancePage(ctx context.Context, creds provider.Credentials, query url.Values) (instancePage, error) {
	endpoint, err := instanceListURL(creds.EndpointOr(d.baseURL), query)
	if err != nil {
		return instancePage{}, fmt.Errorf("vast: instance list URL: %w", err)
	}
	resp, err := d.doRequest(ctx, creds, http.MethodGet, endpoint, nil)
	if err != nil {
		return instancePage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return instancePage{}, fmt.Errorf("vast: list instances: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	page, err := decodeInstancePage(resp.Body)
	if err != nil {
		return instancePage{}, fmt.Errorf("vast: decode instance page: %w", err)
	}
	return page, nil
}

func decodeInstancePage(r io.Reader) (instancePage, error) {
	var wire struct {
		Success   *bool           `json:"success"`
		Instances []instance      `json:"instances"`
		Found     *int            `json:"instances_found"`
		Total     *int            `json:"total_instances"`
		NextToken json.RawMessage `json:"next_token"`
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return instancePage{}, err
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return instancePage{}, err
	}
	if wire.Success == nil || !*wire.Success {
		return instancePage{}, errors.New("instance listing did not report success")
	}
	if wire.Instances == nil || wire.Found == nil || wire.Total == nil || len(wire.NextToken) == 0 {
		return instancePage{}, errors.New("incomplete instance page")
	}
	if *wire.Found != len(wire.Instances) || *wire.Total < len(wire.Instances) {
		return instancePage{}, errors.New("inconsistent instance page counts")
	}
	var next *string
	if err := json.Unmarshal(wire.NextToken, &next); err != nil {
		return instancePage{}, fmt.Errorf("invalid pagination cursor: %w", err)
	}
	page := instancePage{Instances: wire.Instances, Total: *wire.Total}
	if next != nil {
		if *next == "" {
			return instancePage{}, errors.New("empty pagination cursor; expected null at end of listing")
		}
		page.NextToken = *next
	}
	return page, nil
}

// Sync treats a successful listing as the complete inventory. Never expose a
// partial result when a later request fails or pagination stops making progress.
func (d *Driver) listAllInstances(ctx context.Context, creds provider.Credentials) ([]instance, error) {
	query := url.Values{
		"limit":    {strconv.Itoa(instancePageSize)},
		"order_by": {`[{"col":"id","dir":"asc"}]`},
	}
	out := []instance{}
	seenIDs := map[string]bool{}
	seenTokens := map[string]bool{}
	for {
		page, err := d.listInstancePage(ctx, creds, query)
		if err != nil {
			return nil, err
		}
		for _, in := range page.Instances {
			id := in.ID.String()
			if id == "" || seenIDs[id] {
				return nil, errors.New("vast: missing or repeated instance ID in listing")
			}
			seenIDs[id] = true
			out = append(out, in)
		}
		if len(out) > page.Total {
			return nil, errors.New("vast: instance listing exceeds reported total")
		}
		if page.NextToken == "" {
			if len(out) != page.Total {
				return nil, fmt.Errorf("vast: incomplete instance listing: received %d of %d", len(out), page.Total)
			}
			return out, nil
		}
		if len(page.Instances) == 0 || seenTokens[page.NextToken] {
			return nil, errors.New("vast: instance pagination is not making progress")
		}
		seenTokens[page.NextToken] = true
		query.Set("after_token", page.NextToken)
	}
}

func (d *Driver) instanceByID(ctx context.Context, creds provider.Credentials, externalID string) (instance, error) {
	id, err := strconv.ParseUint(externalID, 10, 64)
	if err != nil {
		return instance{}, fmt.Errorf("vast: invalid instance id %q", externalID)
	}
	query := url.Values{
		"limit":          {"1"},
		"select_filters": {fmt.Sprintf(`{"id":{"eq":%d}}`, id)},
	}
	page, err := d.listInstancePage(ctx, creds, query)
	if err != nil {
		return instance{}, err
	}
	if len(page.Instances) == 0 && page.Total == 0 && page.NextToken == "" {
		return instance{}, fmt.Errorf("vast: instance %q not found", externalID)
	}
	if len(page.Instances) != 1 || page.Total != 1 || page.NextToken != "" || page.Instances[0].ID.String() != strconv.FormatUint(id, 10) {
		return instance{}, errors.New("vast: instance query returned an inconsistent result")
	}
	return page.Instances[0], nil
}

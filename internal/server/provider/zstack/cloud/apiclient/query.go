package apiclient

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// QueryAll reads every inventory page or returns an error without partial data.
func QueryAll[T any](ctx context.Context, c *APIClient, path string) ([]T, error) {
	const pageSize = 100
	queryURL, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("zstack: invalid discovery query: %w", err)
	}
	query := queryURL.Query()
	query.Set("limit", strconv.Itoa(pageSize))
	query.Set("sort", "uuid")
	out := []T{}
	for start := 0; start < 100000; start += pageSize {
		var page struct {
			Inventories *[]T `json:"inventories"`
		}
		query.Set("start", strconv.Itoa(start))
		queryURL.RawQuery = query.Encode()
		if err := c.Get(ctx, queryURL.String(), &page); err != nil {
			return nil, err
		}
		if page.Inventories == nil {
			return nil, fmt.Errorf("zstack: missing inventories")
		}
		out = append(out, (*page.Inventories)...)
		if len(*page.Inventories) < pageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("zstack: discovery pagination exceeded safety limit")
}

// QueryResource requires exactly one inventory matching the query.
func QueryResource[T any](ctx context.Context, c *APIClient, path, id string) (T, error) {
	var zero T
	var reply struct {
		Inventories []T `json:"inventories"`
	}
	query := url.Values{"q": {"uuid=" + id}}
	if err := c.Get(ctx, path+"?"+query.Encode(), &reply); err != nil {
		return zero, err
	}
	if len(reply.Inventories) != 1 {
		return zero, fmt.Errorf("zstack: expected one resource from %s for UUID %s, got %d", path, id, len(reply.Inventories))
	}
	return reply.Inventories[0], nil
}

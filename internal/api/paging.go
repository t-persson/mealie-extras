package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// paginate follows Mealie's "next" links using only their query string, never
// their host or path, so authorization is never forwarded elsewhere.
func paginate[T any](ctx context.Context, c *Client, op, path string, query url.Values) ([]T, error) {
	results := make([]T, 0)
	seen := make(map[string]bool)
	for {
		encoded := query.Encode()
		if seen[encoded] {
			return nil, fmt.Errorf("%s: repeated pagination query", op)
		}
		seen[encoded] = true
		var page *struct {
			Items json.RawMessage `json:"items"`
			Next  *string         `json:"next"`
		}
		if err := c.json(ctx, http.MethodGet, path+"?"+encoded, nil, &page, false); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if page == nil {
			return nil, fmt.Errorf("%s: expected JSON object", op)
		}
		// Missing items means an empty page in the Python paging implementation.
		if len(page.Items) != 0 {
			var items []T
			if err := json.Unmarshal(page.Items, &items); err != nil {
				return nil, fmt.Errorf("%s: decode items: %w", op, err)
			}
			if items == nil {
				return nil, fmt.Errorf("%s: items must be an array", op)
			}
			results = append(results, items...)
		}
		if page.Next == nil {
			return results, nil
		}
		next, err := url.Parse(*page.Next)
		if err != nil {
			return nil, fmt.Errorf("%s: parse next URL: %w", op, err)
		}
		parsed, err := url.ParseQuery(next.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("%s: parse next query: %w", op, err)
		}
		// Python passes "None" for unset parameters; drop them.
		query = make(url.Values)
		for key, values := range parsed {
			for _, value := range values {
				if !strings.EqualFold(value, "none") {
					query.Add(key, value)
				}
			}
		}
	}
}

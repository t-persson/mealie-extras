// Package api provides the shared Mealie and ntfy HTTP clients.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	MealieURL = "https://mealie.tp-softworks.se/api"
	NtfyURL   = "https://ntfy.tp-softworks.se"
)

// MealPlanRecipe contains the recipe fields used by the command workflows.
type MealPlanRecipe struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type MealPlan struct {
	ID       int            `json:"id"`
	RecipeID string         `json:"recipeId"`
	Date     string         `json:"date"`
	Recipe   MealPlanRecipe `json:"recipe"`
}

// UnmarshalJSON rejects missing required fields, as the Python model does.
func (m *MealPlan) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID       *int    `json:"id"`
		RecipeID *string `json:"recipeId"`
		Date     *string `json:"date"`
		Recipe   *struct {
			Name *string `json:"name"`
			Slug *string `json:"slug"`
		} `json:"recipe"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ID == nil || wire.RecipeID == nil || wire.Date == nil ||
		wire.Recipe == nil || wire.Recipe.Name == nil || wire.Recipe.Slug == nil {
		return fmt.Errorf("meal plan requires id, recipeId, date, and recipe name/slug")
	}
	*m = MealPlan{
		ID: *wire.ID, RecipeID: *wire.RecipeID, Date: *wire.Date,
		Recipe: MealPlanRecipe{Name: *wire.Recipe.Name, Slug: *wire.Recipe.Slug},
	}
	return nil
}

// Recipe preserves complete ingredients, including fields not known to this client.
type Recipe struct {
	ID          string            `json:"id"`
	Ingredients []json.RawMessage `json:"recipeIngredient"`
}

// UnmarshalJSON validates the fields accessed directly by Python's shopping client.
func (r *Recipe) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID          *string            `json:"id"`
		Ingredients *[]json.RawMessage `json:"recipeIngredient"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ID == nil || wire.Ingredients == nil {
		return fmt.Errorf("recipe requires id and recipeIngredient")
	}
	*r = Recipe{ID: *wire.ID, Ingredients: *wire.Ingredients}
	return nil
}

type Client struct {
	apiKey    string
	ntfyToken string
	mealieURL string
	ntfyURL   string
	http      *http.Client
}

func NewClient(apiKey string) *Client {
	return NewClientWithTokens(apiKey, apiKey)
}

func NewClientWithTokens(apiKey, ntfyToken string) *Client {
	return NewClientWithURLsAndTokens(apiKey, ntfyToken, MealieURL, NtfyURL, nil)
}

// NewClientWithURLs accepts an injectable HTTP client. A nil client uses the
// standard client with no timeout; callers can bound requests with their context.
// Cross-origin redirects are rejected to prevent forwarding bearer credentials.
func NewClientWithURLs(apiKey, mealieURL, ntfyURL string, httpClient *http.Client) *Client {
	return NewClientWithURLsAndTokens(apiKey, apiKey, mealieURL, ntfyURL, httpClient)
}

// NewClientWithURLsAndTokens accepts separate Mealie and ntfy credentials.
func NewClientWithURLsAndTokens(apiKey, ntfyToken, mealieURL, ntfyURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	cloned := *httpClient
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		original := via[0].URL
		if !strings.EqualFold(req.URL.Host, original.Host) || !strings.EqualFold(req.URL.Scheme, original.Scheme) {
			return fmt.Errorf("cross-origin redirect refused")
		}
		if httpClient.CheckRedirect != nil {
			return httpClient.CheckRedirect(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return &Client{
		apiKey: apiKey, ntfyToken: ntfyToken,
		mealieURL: strings.TrimRight(mealieURL, "/"),
		ntfyURL:   strings.TrimRight(ntfyURL, "/"), http: &cloned,
	}
}

func (c *Client) MealPlans(ctx context.Context, startDate, endDate string) ([]MealPlan, error) {
	query := url.Values{"perPage": {"14"}}
	if startDate != "" {
		query.Set("start_date", startDate)
	}
	if endDate != "" {
		query.Set("end_date", endDate)
	}
	return paginate[MealPlan](ctx, c, "list meal plans", "/households/mealplans", query)
}

func (c *Client) RandomMealPlan(ctx context.Context, date string) (MealPlan, error) {
	var plan MealPlan
	err := c.json(ctx, http.MethodPost, "/households/mealplans/random",
		map[string]string{"date": date, "entryType": "dinner"}, &plan, false)
	if err != nil {
		return MealPlan{}, fmt.Errorf("create random meal plan: %w", err)
	}
	return plan, nil
}

func (c *Client) DeleteMealPlan(ctx context.Context, id int) error {
	if err := c.json(ctx, http.MethodDelete, "/households/mealplans/"+strconv.Itoa(id), nil, nil, true); err != nil {
		return fmt.Errorf("delete meal plan %d: %w", id, err)
	}
	return nil
}

func (c *Client) Recipe(ctx context.Context, slug string) (Recipe, error) {
	var recipe Recipe
	if err := c.json(ctx, http.MethodGet, "/recipes/"+url.PathEscape(slug), nil, &recipe, false); err != nil {
		return Recipe{}, fmt.Errorf("get recipe %q: %w", slug, err)
	}
	return recipe, nil
}

func (c *Client) AddRecipes(ctx context.Context, listID string, recipes []Recipe) error {
	type addition struct {
		Quantity    int               `json:"recipeIncrementQuantity"`
		Ingredients []json.RawMessage `json:"recipeIngredients"`
		RecipeID    string            `json:"recipeId"`
	}
	payload := make([]addition, 0, len(recipes))
	for _, recipe := range recipes {
		if recipe.Ingredients == nil {
			return fmt.Errorf("add recipes: recipe %q requires recipeIngredient array", recipe.ID)
		}
		ingredients := make([]json.RawMessage, 0, len(recipe.Ingredients))
		for i, ingredient := range recipe.Ingredients {
			keep, err := keepIngredient(ingredient)
			if err != nil {
				return fmt.Errorf("add recipes: recipe %q ingredient %d: %w", recipe.ID, i, err)
			}
			if keep {
				ingredients = append(ingredients, ingredient)
			}
		}
		payload = append(payload, addition{Quantity: 1, Ingredients: ingredients, RecipeID: recipe.ID})
	}
	if err := c.json(ctx, http.MethodPost, "/households/shopping/lists/"+url.PathEscape(listID)+"/recipe", payload, nil, false); err != nil {
		return fmt.Errorf("add recipes to list %q: %w", listID, err)
	}
	return nil
}

func keepIngredient(raw json.RawMessage) (bool, error) {
	var ingredient map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ingredient); err != nil {
		return false, fmt.Errorf("decode ingredient: %w", err)
	}
	if ingredient == nil {
		return false, fmt.Errorf("ingredient must be an object")
	}
	food := ingredient["food"]
	if len(food) == 0 || bytes.Equal(bytes.TrimSpace(food), []byte("null")) {
		return true, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(food, &fields); err != nil {
		return false, fmt.Errorf("decode food: %w", err)
	}
	var households []string
	if err := json.Unmarshal(fields["householdsWithIngredientFood"], &households); err != nil {
		return false, fmt.Errorf("food requires householdsWithIngredientFood array: %w", err)
	}
	if households == nil {
		return false, fmt.Errorf("food requires householdsWithIngredientFood array")
	}
	for _, household := range households {
		if household == "annika-tobias" {
			return false, nil
		}
	}
	return true, nil
}

// Notify publishes plain message data, not JSON, to the fixed "mealie" topic.
// Like Python's ntfy client it checks status but does not decode the response.
func (c *Client) Notify(ctx context.Context, title, message, click string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ntfyURL+"/mealie", strings.NewReader(message))
	if err != nil {
		return fmt.Errorf("notify: create request: %w", err)
	}
	req.Header.Set("Title", title)
	req.Header.Set("Click", click)
	resp, err := c.doWithToken(req, c.ntfyToken)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("notify: read response: %w", err)
	}
	return nil
}

func (c *Client) json(ctx context.Context, method, path string, body, result any, allowNoContent bool) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode request: %w", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.mealieURL+path, reader)
	if err != nil {
		return fmt.Errorf("%s %s: create request: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// Unlike Python's unconditional .json(), DELETE accepts a 204 response.
	if allowNoContent && resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if result == nil {
		result = new(json.RawMessage)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	return c.doWithToken(req, c.apiKey)
}

func (c *Client) doWithToken(req *http.Request, token string) (*http.Response, error) {
	if token == "" {
		req.Header.Del("Authorization")
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp, nil
}

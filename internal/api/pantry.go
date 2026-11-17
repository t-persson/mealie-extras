package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Ref is a Mealie food or unit reference. ID is empty for unsaved entities.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ShoppingItem is a shopping-list item as needed by the pantry companion.
type ShoppingItem struct {
	ID             string  `json:"id"`
	ShoppingListID string  `json:"shoppingListId"`
	Checked        bool    `json:"checked"`
	Quantity       float64 `json:"quantity"`
	Food           *Ref    `json:"food"`
	Unit           *Ref    `json:"unit"`
	Note           string  `json:"note"`
	Display        string  `json:"display"`
}

// TimelineEvent is a recipe timeline event. Mealie's "I Made This" dialog
// creates events with EventType "comment".
type TimelineEvent struct {
	ID        string `json:"id"`
	RecipeID  string `json:"recipeId"`
	Subject   string `json:"subject"`
	EventType string `json:"eventType"`
	Timestamp string `json:"timestamp"`
}

// PlanEntry is a meal-plan entry that may be a note without a recipe.
type PlanEntry struct {
	ID        int         `json:"id"`
	Date      string      `json:"date"`
	EntryType string      `json:"entryType"`
	Title     string      `json:"title"`
	RecipeID  string      `json:"recipeId"`
	Recipe    *PlanRecipe `json:"recipe"`
}

type PlanRecipe struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Ingredient is a typed recipe ingredient.
type Ingredient struct {
	Quantity *float64 `json:"quantity"`
	Unit     *Ref     `json:"unit"`
	Food     *Ref     `json:"food"`
	Note     string   `json:"note"`
	Display  string   `json:"display"`
}

func (c *Client) ShoppingListItems(ctx context.Context, listID string) ([]ShoppingItem, error) {
	var list *struct {
		Items *[]ShoppingItem `json:"listItems"`
	}
	if err := c.json(ctx, http.MethodGet, "/households/shopping/lists/"+url.PathEscape(listID), nil, &list, false); err != nil {
		return nil, fmt.Errorf("get shopping list %q: %w", listID, err)
	}
	if list == nil || list.Items == nil {
		return nil, fmt.Errorf("get shopping list %q: response requires listItems array", listID)
	}
	return *list.Items, nil
}

// TimelineEvents lists events created at or after since. Filtering on creation
// rather than timestamp catches "I Made This" entries that were backdated.
func (c *Client) TimelineEvents(ctx context.Context, since time.Time) ([]TimelineEvent, error) {
	query := url.Values{
		"perPage": {"100"}, "orderBy": {"createdAt"}, "orderDirection": {"asc"},
		"queryFilter": {fmt.Sprintf("createdAt >= %q", since.UTC().Format("2006-01-02T15:04:05"))},
	}
	return paginate[TimelineEvent](ctx, c, "list timeline events", "/recipes/timeline/events", query)
}

func (c *Client) PlanEntries(ctx context.Context, startDate, endDate string) ([]PlanEntry, error) {
	query := url.Values{"perPage": {"100"}, "start_date": {startDate}, "end_date": {endDate}}
	return paginate[PlanEntry](ctx, c, "list meal plan entries", "/households/mealplans", query)
}

func (c *Client) PlanEntry(ctx context.Context, id int) (PlanEntry, error) {
	var entry PlanEntry
	if err := c.json(ctx, http.MethodGet, "/households/mealplans/"+strconv.Itoa(id), nil, &entry, false); err != nil {
		return PlanEntry{}, fmt.Errorf("get meal plan entry %d: %w", id, err)
	}
	return entry, nil
}

func (c *Client) RecipeIngredients(ctx context.Context, slug string) ([]Ingredient, error) {
	var recipe *struct {
		Ingredients *[]Ingredient `json:"recipeIngredient"`
	}
	if err := c.json(ctx, http.MethodGet, "/recipes/"+url.PathEscape(slug), nil, &recipe, false); err != nil {
		return nil, fmt.Errorf("get recipe %q: %w", slug, err)
	}
	if recipe == nil || recipe.Ingredients == nil {
		return nil, fmt.Errorf("get recipe %q: response requires recipeIngredient array", slug)
	}
	return *recipe.Ingredients, nil
}

func (c *Client) Foods(ctx context.Context) ([]Ref, error) {
	query := url.Values{"perPage": {"200"}, "orderBy": {"name"}, "orderDirection": {"asc"}}
	return paginate[Ref](ctx, c, "list foods", "/foods", query)
}

func (c *Client) Units(ctx context.Context) ([]Ref, error) {
	query := url.Values{"perPage": {"200"}, "orderBy": {"name"}, "orderDirection": {"asc"}}
	return paginate[Ref](ctx, c, "list units", "/units", query)
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const planJSON = `{"id":42,"recipeId":"recipe-id","date":"2026-11-09","recipe":{"name":"Soup","slug":"soup"}}`

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClientWithURLs("test-key", server.URL+"/api/", server.URL+"/", server.Client())
}

func checkRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method || r.URL.EscapedPath() != path {
		t.Errorf("request = %s %s; want %s %s", r.Method, r.URL.EscapedPath(), method, path)
	}
	if got := r.Header.Get("Authorization"); got != "bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request: %v", err)
	}
	return string(data)
}

func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Errorf("invalid JSON %q: %v", got, err)
		return
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON = %s; want %s", got, want)
	}
}

func TestMealPlansPagination(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		t.Error("pagination followed a foreign host")
	}))
	defer foreign.Close()
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, http.MethodGet, "/api/households/mealplans")
		switch calls.Add(1) {
		case 1:
			want := url.Values{"perPage": {"14"}, "start_date": {"2026-11-09"}, "end_date": {"2026-11-15"}}
			if got := r.URL.Query(); !reflect.DeepEqual(got, want) {
				t.Errorf("query = %v; want %v", got, want)
			}
			next := foreign.URL + "/wrong-route?page=2&perPage=14&start_date=NoNe&end_date=none&queryFilter=a%2Bb%26c%3Dd+e&tag=x&tag=y&tag=NONE"
			json.NewEncoder(w).Encode(map[string]any{
				"items": []json.RawMessage{json.RawMessage(planJSON)}, "next": next,
			})
		case 2:
			want := url.Values{"page": {"2"}, "perPage": {"14"}, "queryFilter": {"a+b&c=d e"}, "tag": {"x", "y"}}
			if got := r.URL.Query(); !reflect.DeepEqual(got, want) {
				t.Errorf("next query = %v; want %v", got, want)
			}
			fmt.Fprint(w, `{"items":[`+planJSON+`],"next":"?page=3&perPage=14"}`)
		case 3:
			if got := r.URL.Query().Get("page"); got != "3" {
				t.Errorf("page = %q", got)
			}
			fmt.Fprint(w, `{"items":[],"next":null}`)
		default:
			t.Error("unexpected page")
			fmt.Fprint(w, `{}`)
		}
	})
	plans, err := client.MealPlans(context.Background(), "2026-11-09", "2026-11-15")
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].ID != 42 || plans[0].RecipeID != "recipe-id" ||
		plans[0].Date != "2026-11-09" || plans[0].Recipe.Name != "Soup" || plans[0].Recipe.Slug != "soup" {
		t.Fatalf("plans = %+v", plans)
	}
	if calls.Load() != 3 || foreignCalls.Load() != 0 {
		t.Fatalf("calls = %d, foreign calls = %d", calls.Load(), foreignCalls.Load())
	}
}

func TestMealPlansOptionalDatesAndMissingItems(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, http.MethodGet, "/api/households/mealplans")
		if got := r.URL.RawQuery; got != "perPage=14" {
			t.Errorf("query = %q", got)
		}
		fmt.Fprint(w, `{}`)
	})
	plans, err := client.MealPlans(context.Background(), "", "")
	if err != nil || len(plans) != 0 {
		t.Fatalf("plans = %v, error = %v", plans, err)
	}
}

func TestMealPlansInvalidResponses(t *testing.T) {
	for _, body := range []string{
		`not json`, `null`, `[]`, `{"items":null}`, `{"items":{}}`, `{"items":[{}]}`,
		`{"items":[null]}`, `{"items":[{"id":"wrong"}]}`, `{"next":5}`,
		`{"next":"?page=%zz"}`, `{"next":"?perPage=14"}`, `{} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			})
			if _, err := client.MealPlans(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "list meal plans") {
				t.Fatalf("expected contextual error, got %v", err)
			}
		})
	}
}

func TestRandomMealPlan(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, http.MethodPost, "/api/households/mealplans/random")
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		assertJSON(t, readBody(t, r), `{"date":"2026-11-09","entryType":"dinner"}`)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, planJSON)
	})
	plan, err := client.RandomMealPlan(context.Background(), "2026-11-09")
	if err != nil || plan.ID != 42 || plan.Recipe.Slug != "soup" {
		t.Fatalf("plan = %+v, error = %v", plan, err)
	}
}

func TestMealPlanRequiredFields(t *testing.T) {
	for _, field := range []string{"id", "recipeId", "date", "recipe", "name", "slug"} {
		t.Run(field, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(planJSON), &fields); err != nil {
				t.Fatal(err)
			}
			if field == "name" || field == "slug" {
				var recipe map[string]json.RawMessage
				if err := json.Unmarshal(fields["recipe"], &recipe); err != nil {
					t.Fatal(err)
				}
				delete(recipe, field)
				fields["recipe"], _ = json.Marshal(recipe)
			} else {
				delete(fields, field)
			}
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(fields)
			})
			if _, err := client.RandomMealPlan(context.Background(), "2026-11-09"); err == nil {
				t.Fatalf("expected error for missing %s", field)
			}
		})
	}
}

func TestDeleteMealPlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		fail   bool
	}{
		{"json", 200, `{}`, false},
		{"no content", 204, "", false},
		{"empty 200", 200, "", true},
		{"invalid JSON", 200, "invalid", true},
		{"not found", 404, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				checkRequest(t, r, http.MethodDelete, "/api/households/mealplans/42")
				if got := readBody(t, r); got != "" {
					t.Errorf("unexpected body %q", got)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			err := client.DeleteMealPlan(context.Background(), 42)
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, want failure %v", err, tc.fail)
			}
		})
	}
}

func TestRecipeAndShoppingRawIngredients(t *testing.T) {
	ingredients := []string{
		`{"note":"no food","quantity":1.25,"unit":{"id":"u","extra":true},"custom":[1,2],"large":9007199254740993}`,
		`{"food":null,"note":"null food"}`,
		`{"food":{"id":"food-1","householdsWithIngredientFood":["other"]},"extra":{"nested":"kept"}}`,
		`{"food":{"householdsWithIngredientFood":[]},"quantity":0}`,
		`{"food":{"householdsWithIngredientFood":["other","annika-tobias"]},"note":"excluded"}`,
	}
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			checkRequest(t, r, http.MethodGet, "/api/recipes/soup%20%2F%3F")
			fmt.Fprintf(w, `{"id":"r1","recipeIngredient":[%s],"unknown":"ignored"}`, strings.Join(ingredients, ","))
		case 2:
			checkRequest(t, r, http.MethodPost, "/api/households/shopping/lists/list%20%2F%3F/recipe")
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON content type")
			}
			body := readBody(t, r)
			want := `[{"recipeIncrementQuantity":1,"recipeId":"r1","recipeIngredients":[` + strings.Join(ingredients[:4], ",") + `]}]`
			assertJSON(t, body, want)
			if !strings.Contains(body, "9007199254740993") {
				t.Error("raw number lost precision")
			}
			fmt.Fprint(w, `{}`)
		default:
			t.Error("unexpected call")
		}
	})
	recipe, err := client.Recipe(context.Background(), "soup /?")
	if err != nil {
		t.Fatal(err)
	}
	if recipe.ID != "r1" || len(recipe.Ingredients) != 5 {
		t.Fatalf("recipe = %+v", recipe)
	}
	if err := client.AddRecipes(context.Background(), "list /?", []Recipe{recipe}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestRecipeRequiredFields(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"recipeIngredient":[]}`, `{"id":"r"}`,
		`{"id":null,"recipeIngredient":[]}`, `{"id":5,"recipeIngredient":[]}`,
		`{"id":"r","recipeIngredient":null}`, `{"id":"r","recipeIngredient":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			})
			if _, err := client.Recipe(context.Background(), "soup"); err == nil || !strings.Contains(err.Error(), "get recipe") {
				t.Fatalf("expected contextual error, got %v", err)
			}
		})
	}
}

func TestAddRecipesInvalidIngredients(t *testing.T) {
	for _, ingredient := range []string{
		`null`, `[]`, `"bad"`, `{`, `{"food":{}}`,
		`{"food":{"householdsWithIngredientFood":null}}`,
		`{"food":{"householdsWithIngredientFood":5}}`, `{"food":[]}`,
	} {
		t.Run(ingredient, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("invalid ingredient must fail before HTTP")
			})
			err := client.AddRecipes(context.Background(), "list", []Recipe{
				{ID: "r", Ingredients: []json.RawMessage{json.RawMessage(ingredient)}},
			})
			if err == nil || !strings.Contains(err.Error(), `recipe "r" ingredient 0`) {
				t.Fatalf("expected contextual error, got %v", err)
			}
		})
	}
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing ingredients must fail before HTTP")
	})
	if err := client.AddRecipes(context.Background(), "list", []Recipe{{ID: "r"}}); err == nil {
		t.Fatal("expected missing ingredients error")
	}
}

func TestAddRecipesEmptyArrays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recipes []Recipe
		want    string
	}{
		{"nil recipes", nil, `[]`},
		{"empty recipes", []Recipe{}, `[]`},
		{"empty ingredients", []Recipe{{ID: "r", Ingredients: []json.RawMessage{}}}, `[{"recipeIncrementQuantity":1,"recipeIngredients":[],"recipeId":"r"}]`},
		{"filtered ingredients", []Recipe{{ID: "r", Ingredients: []json.RawMessage{json.RawMessage(`{"food":{"householdsWithIngredientFood":["annika-tobias"]}}`)}}}, `[{"recipeIncrementQuantity":1,"recipeIngredients":[],"recipeId":"r"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				checkRequest(t, r, http.MethodPost, "/api/households/shopping/lists/list/recipe")
				assertJSON(t, readBody(t, r), tc.want)
				fmt.Fprint(w, `[]`)
			})
			if err := client.AddRecipes(context.Background(), "list", tc.recipes); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNotify(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		checkRequest(t, r, http.MethodPost, "/mealie")
		if r.Header.Get("Title") != "New meal plan" || r.Header.Get("Click") != "https://example.test/plan" {
			t.Errorf("headers = %v", r.Header)
		}
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Errorf("unexpected Content-Type = %q", got)
		}
		if got := readBody(t, r); got != "Message with åäö\nnot JSON" {
			t.Errorf("body = %q", got)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, "not JSON")
	})
	if err := client.Notify(context.Background(), "New meal plan", "Message with åäö\nnot JSON", "https://example.test/plan"); err != nil {
		t.Fatal(err)
	}
}

func TestStatusAndJSONErrors(t *testing.T) {
	operations := map[string]func(*Client) error{
		"meal plans": func(c *Client) error { _, err := c.MealPlans(context.Background(), "", ""); return err },
		"random":     func(c *Client) error { _, err := c.RandomMealPlan(context.Background(), "date"); return err },
		"recipe":     func(c *Client) error { _, err := c.Recipe(context.Background(), "slug"); return err },
		"shopping":   func(c *Client) error { return c.AddRecipes(context.Background(), "list", nil) },
		"notify":     func(c *Client) error { return c.Notify(context.Background(), "", "", "") },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{}`)
			})
			if err := operation(client); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
				t.Fatalf("expected HTTP error, got %v", err)
			}
			if name == "notify" {
				return
			}
			for _, body := range []string{"", "not JSON", "{} trailing"} {
				client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, body)
				})
				if err := operation(client); err == nil {
					t.Errorf("expected decode error for %q", body)
				}
			}
		})
	}
}

func TestContextCancellation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled request should not reach server")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.MealPlans(ctx, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
}

func TestNoForeignRedirectAuthorization(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
	}))
	defer foreign.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
	})
	if _, err := client.MealPlans(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "cross-origin redirect refused") {
		t.Fatalf("expected redirect error, got %v", err)
	}
	if foreignCalls.Load() != 0 {
		t.Fatalf("foreign calls = %d", foreignCalls.Load())
	}
}

func TestDefaultsAndNoneKey(t *testing.T) {
	client := NewClient("None")
	if client.mealieURL != MealieURL || client.ntfyURL != NtfyURL || client.http.Timeout != 0 {
		t.Fatalf("unexpected defaults: %+v", client)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer None" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client = NewClientWithURLs("None", server.URL, server.URL, server.Client())
	if _, err := client.MealPlans(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := client.Notify(context.Background(), "", "", ""); err != nil {
		t.Fatal(err)
	}
}

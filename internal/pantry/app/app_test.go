package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t-persson/mealie-extras/internal/cli"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestParseConfigDefaultsAndEnv(t *testing.T) {
	cfg, err := ParseConfig(nil, env(map[string]string{"API_KEY": "k"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "k" || cfg.MealieURL != DefaultMealieURL || cfg.Listen != ":8080" || cfg.DBPath != "pantry.db" ||
		cfg.PollInterval != 5*time.Minute || len(cfg.ListIDs) != 1 || cfg.ListIDs[0] != cli.DefaultListID {
		t.Fatalf("defaults %+v", cfg)
	}

	cfg, err = ParseConfig(
		[]string{"--apikey", "flag", "--list", "a", "--list", "b,c", "--mealie-url", "http://m.local/api/"},
		env(map[string]string{"API_KEY": "env", "SHOPPING_LISTS": "x", "POLL_INTERVAL": "30s", "TZ": "Europe/Stockholm", "HOOK_SECRET": "h"}),
		io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "flag" || strings.Join(cfg.ListIDs, ",") != "a,b,c" || cfg.MealieURL != "http://m.local" ||
		cfg.PollInterval != 30*time.Second || cfg.Location.String() != "Europe/Stockholm" || cfg.HookSecret != "h" {
		t.Fatalf("explicit/env %+v", cfg)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "missing Mealie API key"},
		{[]string{"--apikey", "k", "--poll-interval", "0s"}, "poll-interval"},
		{[]string{"--apikey", "k", "--mealie-url", "mealie"}, "mealie-url"},
		{[]string{"--apikey", "k", "--timezone", "Nowhere/City"}, "timezone"},
		{[]string{"--apikey", "k", "extra"}, "unexpected arguments"},
	} {
		if _, err := ParseConfig(tc.args, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.args, err, tc.want)
		}
	}
	if code := Run(context.Background(), []string{"--bogus"}, io.Discard, io.Discard); code == 0 {
		t.Error("bad flag exited zero")
	}
}

func TestServeSyncsAndShutsDown(t *testing.T) {
	requests := make(chan string, 16)
	mealie := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/households/shopping/lists/") {
			io.WriteString(w, `{"id":"l1","listItems":[]}`)
			return
		}
		io.WriteString(w, `{"items":[],"next":null}`)
	}))
	defer mealie.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	cfg := Config{
		APIKey: "k", MealieURL: mealie.URL, Listen: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "p.db"),
		ListIDs: []string{"l1"}, PollInterval: time.Hour, Location: time.UTC, LogLevel: "INFO",
	}
	go func() { done <- Serve(ctx, cfg, io.Discard, ready) }()
	addr := <-ready

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status %d", resp.StatusCode)
	}
	select {
	case got := <-requests:
		if !strings.HasPrefix(got, "GET /api/") {
			t.Fatalf("unexpected Mealie request %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial sync did not reach Mealie")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

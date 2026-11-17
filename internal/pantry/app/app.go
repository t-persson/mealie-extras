// Package app wires the mealie-pantry server: configuration, Mealie client,
// SQLite store, background sync and HTTP interface.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/t-persson/mealie-extras/internal/api"
	"github.com/t-persson/mealie-extras/internal/cli"
	"github.com/t-persson/mealie-extras/internal/pantry/store"
	"github.com/t-persson/mealie-extras/internal/pantry/sync"
	"github.com/t-persson/mealie-extras/internal/pantry/web"
)

const DefaultMealieURL = "https://mealie.tp-softworks.se"

type Config struct {
	APIKey       string
	MealieURL    string
	Listen       string
	DBPath       string
	ListIDs      []string
	PollInterval time.Duration
	HookSecret   string
	Location     *time.Location
	LogLevel     string
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			*l = append(*l, id)
		}
	}
	return nil
}

// ParseConfig reads flags; each flag falls back to its environment variable
// when not given explicitly.
func ParseConfig(args []string, getenv func(string) (string, bool), output io.Writer) (Config, error) {
	var cfg Config
	var lists listFlag
	var timezone string
	flags := flag.NewFlagSet("mealie-pantry", flag.ContinueOnError)
	flags.StringVar(&cfg.APIKey, "apikey", "", "Mealie API key (env API_KEY)")
	flags.StringVar(&cfg.MealieURL, "mealie-url", DefaultMealieURL, "Mealie base URL without /api (env MEALIE_URL)")
	flags.StringVar(&cfg.Listen, "listen", ":8080", "HTTP listen address (env LISTEN)")
	flags.StringVar(&cfg.DBPath, "db", "pantry.db", "SQLite database path (env DB_PATH)")
	flags.Var(&lists, "list", "Shopping list ID to watch; repeatable or comma separated (env SHOPPING_LISTS; default "+cli.DefaultListID+")")
	flags.DurationVar(&cfg.PollInterval, "poll-interval", 5*time.Minute, "Interval between Mealie syncs (env POLL_INTERVAL)")
	flags.StringVar(&cfg.HookSecret, "hook-secret", "", "Secret path segment for the Mealie notifier hook; empty disables it (env HOOK_SECRET)")
	flags.StringVar(&timezone, "timezone", "", "IANA time zone for meal-plan dates (env TZ; default local)")
	flags.StringVar(&cfg.LogLevel, "loglevel", "INFO", "Log level: DEBUG, INFO, WARNING, ERROR, CRITICAL (env LOGLEVEL)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: mealie-pantry [options]")
		flags.PrintDefaults()
	}
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
		}
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for name, env := range map[string]string{
		"apikey": "API_KEY", "mealie-url": "MEALIE_URL", "listen": "LISTEN", "db": "DB_PATH",
		"list": "SHOPPING_LISTS", "poll-interval": "POLL_INTERVAL", "hook-secret": "HOOK_SECRET",
		"timezone": "TZ", "loglevel": "LOGLEVEL",
	} {
		if value, ok := getenv(env); ok && !explicit[name] && value != "" {
			if err := flags.Set(name, value); err != nil {
				return Config{}, fmt.Errorf("invalid %s %q: %w", env, value, err)
			}
		}
	}
	cfg.ListIDs = lists
	if len(cfg.ListIDs) == 0 {
		cfg.ListIDs = []string{cli.DefaultListID}
	}
	if cfg.APIKey == "" {
		return Config{}, errors.New("missing Mealie API key: set --apikey or API_KEY")
	}
	cfg.MealieURL = strings.TrimSuffix(strings.TrimRight(cfg.MealieURL, "/"), "/api")
	if !strings.HasPrefix(cfg.MealieURL, "http://") && !strings.HasPrefix(cfg.MealieURL, "https://") {
		return Config{}, fmt.Errorf("invalid --mealie-url %q: expected http(s) URL", cfg.MealieURL)
	}
	if cfg.PollInterval <= 0 {
		return Config{}, fmt.Errorf("invalid --poll-interval %s: must be positive", cfg.PollInterval)
	}
	switch cfg.LogLevel {
	case "DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL":
	default:
		return Config{}, fmt.Errorf("invalid --loglevel %q (choose DEBUG, INFO, WARNING, ERROR, CRITICAL)", cfg.LogLevel)
	}
	cfg.Location = time.Local
	if timezone != "" {
		loc, err := time.LoadLocation(strings.TrimPrefix(timezone, ":"))
		if err != nil {
			return Config{}, fmt.Errorf("invalid --timezone %q: %w", timezone, err)
		}
		cfg.Location = loc
	}
	return cfg, nil
}

// Run starts the server and blocks until ctx is cancelled or serving fails.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := ParseConfig(args, os.LookupEnv, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "mealie-pantry: %v\n", err)
		return 2
	}
	if err := Serve(ctx, cfg, stderr, nil); err != nil {
		fmt.Fprintf(stderr, "mealie-pantry: %v\n", err)
		return 1
	}
	return 0
}

// Serve runs the application with cfg. If ready is non-nil it receives the
// bound listener address once the server accepts connections.
func Serve(ctx context.Context, cfg Config, logOutput io.Writer, ready chan<- string) error {
	logger := cli.NewLogger(logOutput, cfg.LogLevel, false)
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	client := api.NewClientWithURLs(cfg.APIKey, cfg.MealieURL+"/api", api.NtfyURL, &http.Client{Timeout: 30 * time.Second})
	syncer := &sync.Syncer{Mealie: client, Store: st, ListIDs: cfg.ListIDs, Location: cfg.Location, Logger: logger}
	server := &web.Server{
		Store: st, Catalog: client, Syncer: syncer, HookSecret: cfg.HookSecret,
		MealieURL: cfg.MealieURL, Location: cfg.Location, Logger: logger,
	}
	handler, err := server.Handler()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		syncer.Run(ctx, cfg.PollInterval)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	logger.Info("listening on %s, watching %d shopping list(s), syncing every %s", listener.Addr(), len(cfg.ListIDs), cfg.PollInterval)
	if cfg.HookSecret == "" {
		logger.Info("no hook secret configured; purchases are picked up by polling only")
	}
	if ready != nil {
		ready <- listener.Addr().String()
	}

	select {
	case err = <-serveErr:
		err = fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if serr := httpServer.Shutdown(shutdownCtx); serr != nil {
			err = fmt.Errorf("shut down http: %w", serr)
		}
	}
	cancel()
	<-syncDone
	return err
}

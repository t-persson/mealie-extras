package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultListID = "704f7c3f-da98-4038-84ca-7910d4f8c976"

var ErrHelp = flag.ErrHelp

type Options struct {
	StartDate time.Time
	Num       int
	NumText   string
	APIKey    string
	NtfyToken string
	ListID    string
	LogLevel  string
	CleanText bool
}

func Parse(command string, args []string, now time.Time, output io.Writer) (Options, error) {
	var options Options
	var startDate string
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&startDate, "startdate", "", "Start date (YYYY-MM-DD; defaults to next Monday)")
	flags.StringVar(&options.NumText, "num", "7", "Number of days")
	flags.StringVar(&options.LogLevel, "loglevel", "INFO", "Log level: DEBUG, INFO, WARNING, ERROR, CRITICAL")
	flags.BoolVar(&options.CleanText, "cleantext", false, "Log messages without timestamps or levels")
	flags.StringVar(&options.APIKey, "apikey", "None", "API key (defaults to API_KEY environment variable)")
	flags.StringVar(&options.NtfyToken, "ntfy-token", "", "ntfy token (defaults to NTFY_TOKEN or API_KEY)")
	if command == "mealie-shoppinglist" {
		flags.StringVar(&options.ListID, "list", DefaultListID, "Shopping list ID")
	}
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: %s [options]\n", command)
		flags.PrintDefaults()
	}
	// The flag package prints errors itself; return them for the command's stderr.
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
		}
		return Options{}, err
	}
	if flags.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	switch options.LogLevel {
	case "DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL":
	default:
		return Options{}, fmt.Errorf("invalid --loglevel %q (choose DEBUG, INFO, WARNING, ERROR, CRITICAL)", options.LogLevel)
	}
	if options.NumText == "" || strings.IndexFunc(options.NumText, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return Options{}, fmt.Errorf("invalid --num %q: expected nonnegative digits", options.NumText)
	}
	num, err := strconv.Atoi(options.NumText)
	if err != nil {
		return Options{}, fmt.Errorf("invalid --num %q: %w", options.NumText, err)
	}
	options.Num = num
	explicitKey, explicitNtfyToken, explicitDate := false, false, false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "apikey":
			explicitKey = true
		case "ntfy-token":
			explicitNtfyToken = true
		case "startdate":
			explicitDate = true
		}
	})
	if !explicitDate {
		days := (int(time.Monday) - int(now.Weekday()) + 7) % 7
		if days == 0 {
			days = 7
		}
		options.StartDate = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, days)
	} else {
		options.StartDate, err = time.ParseInLocation("2006-1-2", startDate, now.Location())
		if err != nil {
			return Options{}, fmt.Errorf("invalid --startdate %q: %w", startDate, err)
		}
	}
	if !explicitKey {
		if key, present := os.LookupEnv("API_KEY"); present {
			options.APIKey = key
		}
	}
	if !explicitNtfyToken {
		if token, present := os.LookupEnv("NTFY_TOKEN"); present {
			options.NtfyToken = token
		} else {
			options.NtfyToken = options.APIKey
		}
	}
	return options, nil
}

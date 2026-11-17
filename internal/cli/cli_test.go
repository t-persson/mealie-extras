package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNextMonday(t *testing.T) {
	t.Setenv("API_KEY", "environment-key")
	for _, test := range []struct {
		now  string
		want string
	}{
		{"2026-11-02", "2026-11-09"},
		{"2026-11-03", "2026-11-09"},
		{"2026-11-08", "2026-11-09"},
		{"2026-12-31", "2027-01-04"},
	} {
		t.Run(test.now, func(t *testing.T) {
			now, _ := time.Parse("2006-01-02", test.now)
			options, err := Parse("mealie-shoppinglist", nil, now, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if got := options.StartDate.Format("2006-01-02"); got != test.want {
				t.Fatalf("date = %s, want %s", got, test.want)
			}
			if options.Num != 7 || options.APIKey != "environment-key" || options.NtfyToken != "environment-key" ||
				options.ListID != DefaultListID {
				t.Fatalf("unexpected defaults: %+v", options)
			}
		})
	}
}

func TestExplicitOptions(t *testing.T) {
	t.Setenv("API_KEY", "environment-key")
	options, err := Parse("mealie-shoppinglist", []string{
		"--startdate", "2026-2-3", "--num", "00", "--apikey", "",
		"--ntfy-token", "ntfy-flag", "--list", "another-list", "--cleantext", "--loglevel", "DEBUG",
	}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.StartDate.Format("2006-01-02") != "2026-02-03" || options.Num != 0 || options.NumText != "00" ||
		options.APIKey != "" || options.NtfyToken != "ntfy-flag" || options.ListID != "another-list" ||
		!options.CleanText || options.LogLevel != "DEBUG" {
		t.Fatalf("unexpected options: %+v", options)
	}
}

func TestNtfyTokenEnvironment(t *testing.T) {
	t.Setenv("API_KEY", "mealie-key")
	t.Setenv("NTFY_TOKEN", "ntfy-environment-key")
	options, err := Parse("mealie-generate", nil, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.NtfyToken != "ntfy-environment-key" {
		t.Fatalf("ntfy token = %q", options.NtfyToken)
	}
}

func TestMissingAPIKey(t *testing.T) {
	t.Setenv("API_KEY", "temporary-key")
	if err := os.Unsetenv("API_KEY"); err != nil {
		t.Fatal(err)
	}
	options, err := Parse("mealie-generate", nil, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.APIKey != "None" {
		t.Fatalf("API key = %q, want Python's missing-key representation", options.APIKey)
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--num", "-1"}, {"--num", "+1"}, {"--num", "1.5"}, {"--num", ""},
		{"--num", "999999999999999999999999"}, {"--startdate", "2026-02-30"}, {"--startdate", ""},
		{"--loglevel", "info"}, {"--list", "unavailable"}, {"--unknown"}, {"positional"}, {"--num"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := Parse("mealie-generate", args, time.Now(), &bytes.Buffer{}); err == nil {
				t.Fatal("expected argument error")
			}
		})
	}
}

func TestHelpDoesNotExposeEnvironmentKey(t *testing.T) {
	t.Setenv("API_KEY", "private-environment-key")
	var output bytes.Buffer
	_, err := Parse("mealie-shoppinglist", []string{"--help"}, time.Now(), &output)
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("error = %v, want ErrHelp", err)
	}
	if !strings.Contains(output.String(), "-startdate") {
		t.Fatalf("missing flag help: %s", &output)
	}
	if strings.Contains(output.String(), "private-environment-key") {
		t.Fatal("help exposed the environment API key")
	}
}

func TestTimestampedLogging(t *testing.T) {
	var output bytes.Buffer
	NewLogger(&output, "INFO", false).Info("message")
	line := output.String()
	if !strings.HasSuffix(line, " INFO: message\n") {
		t.Fatalf("unexpected log format: %q", line)
	}
	if _, err := time.ParseInLocation("2006-01-02 15:04:05,000", strings.TrimSuffix(line, " INFO: message\n"), time.Local); err != nil {
		t.Fatalf("unexpected timestamp in %q: %v", line, err)
	}
}

func TestLoggingLevelsAndCleanText(t *testing.T) {
	for _, level := range []string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"} {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			logger := NewLogger(&output, level, true)
			logger.Debug("debug %d", 1)
			logger.Info("info %s", "message")
			want := ""
			if level == "DEBUG" {
				want = "debug 1\ninfo message\n"
			} else if level == "INFO" {
				want = "info message\n"
			}
			if output.String() != want {
				t.Fatalf("logs = %q, want %q", output.String(), want)
			}
		})
	}
}

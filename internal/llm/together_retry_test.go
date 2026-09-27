package llm

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestTogetherRetryHints(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 123456789, time.UTC)
	for name, test := range map[string]struct {
		after, reset []string
		want         time.Time
	}{
		"absent":               {},
		"seconds":              {after: []string{"60"}, want: now.Add(time.Minute)},
		"zero":                 {after: []string{"0"}, want: now},
		"date":                 {after: []string{now.Add(time.Hour).Format(http.TimeFormat)}, want: now.Add(time.Hour).Truncate(time.Second)},
		"reset wins":           {after: []string{"30"}, reset: []string{"60"}, want: now.Add(time.Minute)},
		"after wins":           {after: []string{"120"}, reset: []string{"60"}, want: now.Add(2 * time.Minute)},
		"multiple values":      {after: []string{"2", "120", "10"}, reset: []string{"60", "180"}, want: now.Add(3 * time.Minute)},
		"fraction rounds up":   {reset: []string{"1.00000000000000000001"}, want: now.Add(2 * time.Second)},
		"zero fraction":        {reset: []string{"1.000"}, want: now.Add(time.Second)},
		"small fraction":       {reset: []string{"0.00001"}, want: now.Add(time.Second)},
		"whitespace":           {after: []string{" 60 \t"}, want: now.Add(time.Minute)},
		"invalid":              {after: []string{"-1", "1.5", "secret", "NaN"}, reset: []string{"-1", "NaN", "1..5"}},
		"uint overflow":        {after: []string{strings.Repeat("9", 1000)}, want: latestTogetherRetry},
		"reset overflow":       {reset: []string{strings.Repeat("9", 1000) + ".5"}, want: latestTogetherRetry},
		"duration overflow":    {after: []string{"999999999999"}, want: latestTogetherRetry},
		"long supported delay": {after: []string{"10000000000"}, want: time.Unix(now.Unix()+10000000000, int64(now.Nanosecond())).UTC()},
		"maximum date":         {after: []string{latestTogetherRetry.Format(http.TimeFormat)}, want: latestTogetherRetry.Truncate(time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range test.after {
				headers.Add("Retry-After", value)
			}
			for _, value := range test.reset {
				headers.Add("X-Ratelimit-Reset", value)
			}
			got := togetherRetryNotBefore(headers, now)
			if !got.Equal(test.want) {
				t.Fatalf("got %s, want %s", got, test.want)
			}
			outcome := app.GenerationOutcome{Failure: app.GenerationRateLimited, NotBefore: got}
			if err := outcome.Validate(); err != nil {
				t.Fatal(err)
			}
			if got.After(now.Add(time.Hour)) {
				if _, retry := app.NextGenerationRetry(now, now.Add(time.Hour), 1, 0, outcome.Failure, got, 0); retry {
					t.Fatal("retried before long provider hint")
				}
			}
		})
	}
}

package llm

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Saturation denies any practical retry without exceeding the domain/database
// timestamp range (including settlement's upward microsecond rounding).
var latestTogetherRetry = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)

// togetherRetryNotBefore returns the latest retry instant declared by Together response headers.
// Together documents x-ratelimit-reset as seconds to wait, NOT an epoch:
// https://docs.together.ai/docs/rate-limits (reviewed 2026-09-26).
// Combine every header value by maximum; never cap a valid delay downward.
func togetherRetryNotBefore(headers http.Header, now time.Time) time.Time {
	var latest time.Time
	for _, name := range []string{"Retry-After", "X-Ratelimit-Reset"} {
		for _, value := range headers.Values(name) {
			value = strings.TrimSpace(value)
			var hint time.Time
			if name == "Retry-After" {
				hint, _ = http.ParseTime(value)
			}
			if hint.IsZero() {
				hint = togetherDelay(value, now, name == "X-Ratelimit-Reset")
			}
			if hint.After(latest) {
				latest = hint
			}
		}
	}
	return latest
}

// togetherDelay parses one Together retry-delay header value.
func togetherDelay(value string, now time.Time, fractional bool) time.Time {
	whole, fraction, hasFraction := strings.Cut(value, ".")
	if whole == "" || hasFraction && (!fractional || fraction == "") {
		return time.Time{}
	}
	for _, part := range []string{whole, fraction} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return time.Time{}
			}
		}
	}
	// Validate syntax before handling overflow: enormous valid values must not
	// disappear into the ordinary short local backoff. Round fractions UP.
	seconds, err := strconv.ParseUint(whole, 10, 64)
	maxSeconds := uint64(latestTogetherRetry.Unix() - now.Unix())
	if err != nil || seconds >= maxSeconds {
		return latestTogetherRetry
	}
	if strings.Trim(fraction, "0") != "" {
		seconds++
	}
	hint := time.Unix(now.Unix()+int64(seconds), int64(now.Nanosecond())).UTC()
	if hint.After(latestTogetherRetry) {
		return latestTogetherRetry
	}
	return hint
}

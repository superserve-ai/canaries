package metrics

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Exporter errors can carry endpoint URLs and response bodies, so callers
// should log only what these helpers derive from them, never the error text.

var (
	// "400 Bad Request", as in net/http's Response.Status.
	statusLinePattern = regexp.MustCompile(`\b(\d{3}) `)
	// "status code: 503" or "status_code=503".
	statusCodePattern = regexp.MustCompile(`(?i)\bstatus[ _]code\s*[:=]?\s*(\d{3})\b`)
	// `"code":400` or `"code": 400` in a JSON error body.
	jsonCodePattern = regexp.MustCompile(`"code"\s*:\s*(\d{3})\b`)
)

// SamplingPeriodRejection reports whether err, or any error it wraps or
// joins, is Google's Telemetry API refusing a point because the previous
// point for the same series was written within the maximum sampling period.
// The API answers HTTP 400 FAILED_PRECONDITION with "points were written
// more frequently than the maximum sampling period".
func SamplingPeriodRejection(err error) bool {
	return anyError(err, func(e error) bool {
		text := strings.ToLower(e.Error())
		if strings.Contains(text, "maximum sampling period") {
			return true
		}
		return strings.Contains(text, "failed_precondition") && strings.Contains(text, "sampling")
	})
}

// ExporterStatus returns the HTTP error status carried by an OTLP exporter
// error, such as "400 Bad Request", "status code: 503" or `"code":429` in a
// response body. Only statuses from 400 to 599 are accepted.
func ExporterStatus(err error) (int, bool) {
	var status int
	found := anyError(err, func(e error) bool {
		var ok bool
		status, ok = statusFromText(e.Error())
		return ok
	})
	return status, found
}

func statusFromText(text string) (int, bool) {
	for _, match := range statusLinePattern.FindAllStringSubmatchIndex(text, -1) {
		code, ok := errorStatus(text[match[2]:match[3]])
		if !ok {
			continue
		}
		if reason := http.StatusText(code); reason != "" && strings.HasPrefix(text[match[1]:], reason) {
			return code, true
		}
	}
	for _, pattern := range []*regexp.Regexp{statusCodePattern, jsonCodePattern} {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			if code, ok := errorStatus(match[1]); ok {
				return code, true
			}
		}
	}
	return 0, false
}

func errorStatus(digits string) (int, bool) {
	code, err := strconv.Atoi(digits)
	if err != nil || code < 400 || code > 599 {
		return 0, false
	}
	return code, true
}

// anyError visits err and every error it wraps or joins, depth first, until
// match returns true.
func anyError(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return anyError(wrapped.Unwrap(), match)
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if anyError(child, match) {
				return true
			}
		}
	}
	return false
}

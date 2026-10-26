package shared

import (
	"strings"
	"time"
)

var isoLayouts = []struct {
	layout string
	utc    bool // date-only ISO forms are UTC in JavaScript; date-time without offset is local
}{
	{time.RFC3339Nano, false},
	{"2006-01-02T15:04Z07:00", false},
	{"2006-01-02T15:04:05.999999999", false},
	{"2006-01-02T15:04:05", false},
	{"2006-01-02T15:04", false},
	{"2006-01-02", true},
	{"2006-01", true},
	{"2006", true},
}

// ParseJSDate approximates Date.parse for the ISO 8601 forms the backend and
// Microsoft services emit. ok is false where Date.parse would return NaN.
func ParseJSDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, candidate := range isoLayouts {
		location := time.Local
		if candidate.utc {
			location = time.UTC
		}
		if parsed, err := time.ParseInLocation(candidate.layout, value, location); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// JSISOString formats like Date.prototype.toISOString.
func JSISOString(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

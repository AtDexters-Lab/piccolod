package app

import (
	"strings"
)

var defaultTmpfsMounts = []string{"/tmp", "/run", "/var/run", "/var/tmp"}

func tmpfsMountsFromExtensions(extensions map[string]interface{}) []string {
	if extensions == nil {
		return defaultTmpfsMounts
	}
	raw, ok := extensions["tmpfs"]
	if !ok {
		return defaultTmpfsMounts
	}
	list, ok := raw.([]interface{})
	if !ok {
		return defaultTmpfsMounts
	}

	out := []string{}
	seen := map[string]struct{}{}
	for _, entry := range list {
		s, ok := entry.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || !strings.HasPrefix(s, "/") {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return defaultTmpfsMounts
	}
	return out
}

func tmpfsSizeOpt(limit string) string {
	limit = strings.TrimSpace(limit)
	if limit == "" {
		return ""
	}
	upper := strings.ToUpper(limit)
	type suffix struct {
		raw  string
		unit string
	}
	suffixes := []suffix{
		{raw: "TB", unit: "t"},
		{raw: "GB", unit: "g"},
		{raw: "MB", unit: "m"},
		{raw: "KB", unit: "k"},
		{raw: "B", unit: ""},
	}
	for _, s := range suffixes {
		if !strings.HasSuffix(upper, s.raw) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(upper, s.raw))
		if num == "" {
			return ""
		}
		if s.unit == "" {
			return "size=" + strings.ToLower(num)
		}
		return "size=" + strings.ToLower(num) + s.unit
	}
	return ""
}

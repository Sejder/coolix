package web

import (
	"net/url"
	"sort"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

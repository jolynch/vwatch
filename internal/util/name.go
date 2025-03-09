package util

import (
	"os"
	"strings"
)

func ParseName(name string) (map[string]string) {
	repository := name
	tag := ""
	if strings.Contains(name, ":") {
		idx := strings.LastIndex(name, ":")
		repository = name[:idx]
		tag = name[idx + 1:]
	}
	return map[string]string {
		"name": name,
		"repository": repository,
		"tag": tag,
	}
}

func ExpandPattern(pattern string, params map[string]string) (string) {
	mapper := func(param string) string {
		return params[param]
	}
	return os.Expand(pattern, mapper)
}
package parse

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"
	"text/template"
)

func ParseETagToVersion(etag string) string {
	if len(etag) == 0 {
		return etag
	}
	// Strip the double quotes in the etag if present
	if etag[0] == '"' {
		etag = etag[1:]
	}
	if len(etag) > 0 && etag[len(etag)-1] == '"' {
		etag = etag[:len(etag)-1]
	}

	return etag
}

func ParseName(name string) map[string]string {
	repository := name
	tag := "latest"
	if strings.Contains(name, ":") {
		idx := strings.LastIndex(name, ":")
		repository = name[:idx]
		tag = name[idx+1:]
	}
	return map[string]string{
		"name":       name,
		"repository": repository,
		"tag":        tag,
	}
}

func ParseMatchingHeaders(args []string, prefix string) http.Header {
	headers := make(http.Header)
	for _, env := range args {
		components := strings.SplitN(env, "=", 2)
		key := components[0]
		if strings.HasPrefix(key, prefix) {
			// Handles flag parsing where we have Key: Value
			// OR Environment Variable parsing where we have Key=Value
			var value string
			switch len(components) {
			case 1:
				value = components[0]
			case 2:
				value = components[1]
			default:
				value = ""
			}
			if len(components) > 1 {
				value = components[1]
			}
			parser := textproto.NewReader(bufio.NewReader(strings.NewReader(value)))
			hdr, err := parser.ReadMIMEHeader()
			if err != nil {
				for k, vs := range hdr {
					for _, v := range vs {
						headers.Add(k, v)
					}
				}
			} else {
				slog.Warn(fmt.Sprintf("Skipping malformed header [%s] in env var %s: %s", value, key, err.Error()))
			}

		}
	}
	return headers
}

func ExpandPattern(pattern string, params map[string]string) (res string, err error) {
	tmpl, err := template.New("path").Option("missingkey=error").Parse(pattern)
	if err != nil {
		slog.Error(fmt.Sprintf("Invalid template [%s]: %s", pattern, err.Error()))
		return
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, params)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to template [%s]: %s", pattern, err.Error()))
		return
	}
	return buf.String(), nil
}

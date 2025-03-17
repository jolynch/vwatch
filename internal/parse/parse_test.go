package parse

import (
	"reflect"
	"testing"
)

func TestParseETagToVersion(t *testing.T) {
	if ParseETagToVersion("") != "" {
		t.Errorf("Empty version should be empty")
	}
	if ParseETagToVersion("v123") != "v123" {
		t.Errorf("Version without quotes should be equal")
	}
	if ParseETagToVersion("\"v123\"") != "v123" {
		t.Errorf("Version surrounded by quotes should strip quotes")
	}
}

func TestParseName(t *testing.T) {
	const (
		simple         string = "artifact"
		path           string = "org/repo/artifact"
		pathWithTag    string = "org/repo/artifact:tag"
		pathWithLatest string = "org/repo/artifact:latest"
	)

	verify := func(params map[string]string, name string, repo string, tag string) {
		expected := map[string]string{
			"name":       name,
			"repository": repo,
			"tag":        tag,
		}
		if !reflect.DeepEqual(params, expected) {
			t.Errorf("Expected %s but got %s", expected, params)
		}
	}

	verify(ParseName(simple), simple, simple, "latest")
	verify(ParseName(path), path, path, "latest")
	verify(ParseName(pathWithTag), pathWithTag, path, "tag")
	verify(ParseName(pathWithLatest), pathWithLatest, path, "latest")
}

func TestExpandPath(t *testing.T) {
	params := ParseName("repo/image:tag")
	verify := func(input string, expected string, fail bool) {
		actual, err := ExpandPattern(input, params)
		if err != nil && !fail {
			t.Errorf("Expected success but got %s", err.Error())
		}
		if actual != expected {
			t.Errorf("Expected [%s] but got [%s]", expected, actual)
		}
	}
	verify("/foo/bar", "/foo/bar", false)
	verify("/version/{{.name}}", "/version/repo/image:tag", false)
	verify("/version/{{.name}}", "/version/repo/image:tag", false)
	verify("/v2/{{.repository}}/manifests/{{.tag}}", "/v2/repo/image/manifests/tag", false)
	verify("/v2/{{.na}}/manifests/{{.tag}}", "", true)
}

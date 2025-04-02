package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jolynch/vwatch/internal/conf"
	"github.com/jolynch/vwatch/internal/headers"
	"github.com/jolynch/vwatch/internal/parse"
)

func writeVersion(name string, version string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	buf.Write(body)

	uri := "/v1/version/" + name
	if version != "" {
		params := url.Values{}
		params.Add("version", version)
		uri = fmt.Sprintf("%s?%s", uri, params.Encode())
	}
	request, _ := http.NewRequest("PUT", uri, &buf)
	request.SetPathValue("name", name)
	recorder := httptest.NewRecorder()
	putVersion(recorder, request)
	return recorder
}

func readVersion(name string, version string, timeout time.Duration) *httptest.ResponseRecorder {
	uri := "/v1/version/" + name
	if version != "" {
		params := url.Values{}
		params.Add("version", version)
		params.Add("timeout", timeout.String())
		uri = fmt.Sprintf("%s?%s", uri, params.Encode())
	}

	request, _ := http.NewRequest("GET", uri, nil)
	request.SetPathValue("name", name)
	recorder := httptest.NewRecorder()
	getVersion(recorder, request)
	return recorder
}

func FuzzTestReadPutVersion(f *testing.F) {
	config = conf.FromEnv()
	config.JitterFor = 10 * time.Millisecond

	f.Add("repo/image:tag", "", []byte("123"))
	f.Add("artifact", "123", []byte("456"))
	f.Add("repo/image/deep/image:latest", "v123", []byte("abc"))

	f.Fuzz(func(t *testing.T, name string, version string, data []byte) {
		verifyNotEmpty := func(value string) string {
			if value == "" {
				t.Errorf("Expected nonempty")
			}
			return value
		}

		write := writeVersion(name, "", data)
		writtenEtag := verifyNotEmpty(write.Header().Get(headers.ETag))
		lastModified := verifyNotEmpty(write.Header().Get(headers.LastModified))
		_, err := time.Parse(http.TimeFormat, lastModified)
		if err != nil {
			t.Error(err)
		}

		read := readVersion(name, "", 0*time.Second)
		readEtag := verifyNotEmpty(read.Header().Get(headers.ETag))
		if readEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, readEtag)
		}
		var result bytes.Buffer
		read.Body.WriteTo(&result)
		if !reflect.DeepEqual(result.Bytes(), data) {
			t.Errorf("Expected GET to return data that was PUT")
		}

		// Now test blocking
		start := time.Now()
		blockingRead := readVersion(name, writtenEtag, 10*time.Millisecond)
		blockedEtag := verifyNotEmpty(blockingRead.Header().Get(headers.ETag))
		delta := time.Since(start)
		if blockedEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, blockedEtag)
		}
		if delta.Milliseconds() < 10 {
			t.Errorf("Expected to wait at least 10 milliseconds, waited %dms", delta.Milliseconds())
		}
		if delta.Milliseconds() > 100 {
			t.Errorf("Waited %dms which is much longer than 10", delta.Milliseconds())
		}
	})
}

func BenchmarkConcurrentReaders(b *testing.B) {
	config = conf.FromEnv()
	config.JitterFor = 0 * time.Millisecond

	name := "simple"
	resp := writeVersion(name, "", []byte("test"))
	version := resp.Header().Get(headers.ETag)

	b.Run("1:100000 Blocking", func(b *testing.B) {
		var wg sync.WaitGroup
		for range 100000 {
			wg.Add(1)

			go func() {
				defer wg.Done()
				resp = readVersion(name, version, 100*time.Millisecond)
				if resp.Header().Get(headers.ETag) != version {
					b.Errorf("Should have gotten written version")
				}
			}()
		}
		wg.Wait()
	})
}

func doConcurrentReadWithWrite(b *testing.B, name string, numReaders int) {
	writeVersion(name, "", []byte("test"))
	var wg sync.WaitGroup
	var counter atomic.Int32
	counter.Store(0)
	testDuration := 100 * time.Millisecond

	doWrite := func(i int32) {
		readVersion := fmt.Sprintf("%d", i)
		resp := writeVersion(name, readVersion, []byte(fmt.Sprintf("test-%d", i)))

		hdr := parse.ParseETagToVersion(resp.Header().Get(headers.ETag))
		if hdr != readVersion {
			b.Errorf("Version %s should have been the one we gave %s", hdr, readVersion)
		}
		counter.Store(i)
	}
	doWrite(0)

	// Generate writes spaced
	go func() {
		for i := range 20 {
			go doWrite(int32(i + 1))
			time.Sleep(testDuration / 10)
		}
	}()

	// Generate a ton of concurrent readers who are all trying to read the version
	for range numReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readerDelay := time.Duration(rand.Int63n(testDuration.Milliseconds())) * time.Millisecond
			time.Sleep(readerDelay)

			// Counter is incremented after the set, so we should always be "behind" the
			// writer goroutine
			before := counter.Load()
			resp := readVersion(name, fmt.Sprintf("%d", before), 1*time.Second)
			result := parse.ParseETagToVersion(resp.Header().Get(headers.ETag))

			actual, err := strconv.ParseInt(result, 10, 0)
			if actual < 10 && (err != nil || !(int32(actual) > before)) {
				b.Errorf("Got version %d that wasn't > %d, %s", actual, before, resp.Header().Get(headers.ServerTiming))
			}
		}()
	}
	wg.Wait()
}

func BenchmarkConcurrentReadWithWrite(b *testing.B) {
	config = conf.FromEnv()
	config.JitterFor = 0 * time.Millisecond

	testCases := []struct {
		name       string
		numReaders int
	}{
		{"10k", 10000},
		{"50k", 50000},
		{"100k", 100000},
	}

	for _, testCase := range testCases {
		doConcurrentReadWithWrite(b, "img", testCase.numReaders)
	}
}

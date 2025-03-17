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

		var buf bytes.Buffer
		buf.Write(data)
		request, _ := http.NewRequest("PUT", "/version/", &buf)
		request.SetPathValue("name", name)
		recorder := httptest.NewRecorder()
		putVersion(recorder, request)

		writtenEtag := verifyNotEmpty(recorder.Header().Get(headers.ETag))
		lastModified := verifyNotEmpty(recorder.Header().Get(headers.LastModified))
		_, err := time.Parse(http.TimeFormat, lastModified)
		if err != nil {
			t.Error(err)
		}

		getRequest, _ := http.NewRequest("GET", "/version/", nil)
		getRequest.SetPathValue("name", name)
		getRecorder := httptest.NewRecorder()
		getVersion(getRecorder, getRequest)

		readEtag := verifyNotEmpty(getRecorder.Header().Get(headers.ETag))
		if readEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, readEtag)
		}
		var result bytes.Buffer
		getRecorder.Body.WriteTo(&result)
		if !reflect.DeepEqual(result.Bytes(), data) {
			t.Errorf("Expected GET to return data that was PUT")
		}

		// Now test blocking
		params := url.Values{}
		params.Add("version", writtenEtag)
		params.Add("timeout", "10ms")
		start := time.Now()
		blockRequest, _ := http.NewRequest("GET", "/version?"+params.Encode(), nil)
		blockRequest.SetPathValue("name", name)
		blockRecorder := httptest.NewRecorder()
		getVersion(blockRecorder, blockRequest)
		blockedEtag := verifyNotEmpty(blockRecorder.Header().Get(headers.ETag))
		delta := time.Since(start)
		if blockedEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, blockedEtag)
		}
		if delta.Milliseconds() < 10 {
			t.Errorf("Expected to wait at least 10 milliseconds, waited %dms", delta.Milliseconds())
		}
		if delta.Milliseconds() > 100 {
			t.Errorf("Waited %dms which is much longer than 10 + 10", delta.Milliseconds())
		}
	})
}

func BenchmarkConcurrentReaders(b *testing.B) {
	config = conf.FromEnv()
	config.JitterFor = 0 * time.Millisecond

	name := "simple"
	var buf bytes.Buffer
	buf.Write([]byte("test"))
	request, _ := http.NewRequest("PUT", "/version", &buf)
	request.SetPathValue("name", name)
	recorder := httptest.NewRecorder()
	putVersion(recorder, request)
	version := recorder.Header().Get(headers.ETag)

	b.Run("1:10000 Blocking", func(b *testing.B) {
		var wg sync.WaitGroup
		for range 10000 {
			wg.Add(1)

			doRead := func() {
				defer wg.Done()
				params := url.Values{}
				params.Add("version", version)
				params.Add("timeout", "100ms")
				block, _ := http.NewRequest("GET", "/version?"+params.Encode(), nil)
				block.SetPathValue("name", name)
				recorder := httptest.NewRecorder()
				getVersion(recorder, block)
				if recorder.Header().Get(headers.ETag) != version {
					b.Errorf("Should have gotten original version")
				}
			}
			go doRead()
		}
		wg.Wait()
	})

	b.Run("1:10000 Concurrent Write", func(b *testing.B) {
		var wg sync.WaitGroup
		var counter atomic.Int32
		counter.Store(0)
		name = "img"
		testDuration := 100 * time.Millisecond

		doWrite := func(i int32) {
			buf.Write([]byte(fmt.Sprintf("test-%d", i)))
			params := url.Values{}
			params.Add("version", fmt.Sprintf("%d", i))
			request, _ := http.NewRequest("PUT", "/version?"+params.Encode(), &buf)
			request.SetPathValue("name", name)
			recorder := httptest.NewRecorder()
			putVersion(recorder, request)
			hdr := parse.ParseETagToVersion(recorder.Header().Get(headers.ETag))
			if hdr != params.Get("version") {
				b.Errorf("Version %s should have been the one we gave %s", hdr, params.Get("version"))
			}
			counter.Store(i)
		}
		doWrite(0)

		// Generate 10 writes spaced in time
		writer := func() {
			for i := range 10 {
				go doWrite(int32(i + 1))
				time.Sleep(testDuration / 10)
			}
		}
		go writer()

		// Generate a ton of readers who are all trying to read the version
		for range 10000 {
			wg.Add(1)
			doRead := func() {
				defer wg.Done()
				time.Sleep(time.Duration(rand.Int63n(testDuration.Milliseconds())) * time.Millisecond)

				// Counter is incremented after the set, so we should always be "behind" the
				// writer goroutine
				before := counter.Load()
				params := url.Values{}
				params.Add("version", fmt.Sprintf("%d", before))
				params.Add("timeout", "1s")
				block, _ := http.NewRequest("GET", "/version?"+params.Encode(), nil)
				block.SetPathValue("name", name)
				recorder := httptest.NewRecorder()
				getVersion(recorder, block)

				result := parse.ParseETagToVersion(recorder.Header().Get(headers.ETag))

				actual, err := strconv.ParseInt(result, 10, 0)
				if actual < 10 && (err != nil || !(int32(actual) > before)) {
					b.Errorf("Got version %d that wasn't > %d, %s", actual, before, recorder.Header().Get(headers.ServerTiming))
				}
			}
			go doRead()
		}
		wg.Wait()
	})
}

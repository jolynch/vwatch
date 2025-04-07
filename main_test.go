package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jolynch/vwatch/internal/conf"
	"github.com/jolynch/vwatch/internal/headers"
	"github.com/jolynch/vwatch/internal/parse"
)

// So we can log and assert on either Fuzz or Benchmark
type Tester interface {
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func writeVersion(t Tester, name string, version string, body []byte) (recorder *httptest.ResponseRecorder, err error) {
	var buf bytes.Buffer
	buf.Write(body)

	uri := "/v1/versions/" + url.PathEscape(name)
	if version != "" {
		params := url.Values{}
		params.Add("version", version)
		uri = fmt.Sprintf("%s?%s", uri, params.Encode())
	}
	request, err := http.NewRequest("PUT", uri, &buf)
	if request == nil {
		err = errors.New("Invalid request")
		return
	}
	t.Logf("Writing name=[%s] version=[%s]", name, version)
	request.SetPathValue("name", name)
	recorder = httptest.NewRecorder()
	putVersion(recorder, request)
	return recorder, err
}

func readVersion(t Tester, name string, version string, timeout time.Duration) *httptest.ResponseRecorder {
	uri := "/v1/versions/" + url.PathEscape(name)
	if version != "" {
		params := url.Values{}
		params.Add("version", version)
		params.Add("timeout", timeout.String())
		uri = fmt.Sprintf("%s?%s", uri, params.Encode())
	}
	if timeout > 2*time.Second {
		t.Fatalf("Cannot block reads for >s")
	}

	//t.Logf("GET [%s]", uri)
	request, _ := http.NewRequest("GET", uri, nil)
	request.SetPathValue("name", name)
	recorder := httptest.NewRecorder()

	done := make(chan bool, 1)
	go func() {
		getVersion(recorder, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Should not have blocked for more than 2 seconds")
	}

	return recorder
}

func FuzzReadPutVersion(f *testing.F) {
	config = conf.FromEnv()
	config.JitterPerWatch = 10 * time.Millisecond
	config.DataLimitBytes = 1024
	config.DataLimitError = false

	f.Add("repo/image:tag", "", []byte("123"))
	f.Add("artifact", "123", []byte("456"))
	f.Add("repo/image/deep/image:latest", "v123", []byte("abc"))

	f.Fuzz(func(t *testing.T, name string, version string, data []byte) {
		if !utf8.ValidString(name) || !utf8.ValidString(version) {
			t.Skip()
			return
		}
		verifyNotEmpty := func(value string, ctx string) string {
			if value == "" {
				t.Fatalf("Expected nonempty value for %s", ctx)
			}
			return value
		}

		write, err := writeVersion(t, name, "", data)
		if err != nil {
			// This can only happen if the name can't be put in the path variable
			t.Skipf("Could not construct proper request from name: %s", err.Error())
			return
		}
		t.Logf("Write Response: %d\n", write.Code)
		writtenEtag := verifyNotEmpty(write.Header().Get(headers.ETag), "Write ETag")
		lastModified := verifyNotEmpty(write.Header().Get(headers.LastModified), "Write LastModified")
		_, err = time.Parse(http.TimeFormat, lastModified)
		if err != nil {
			t.Error(err)
		}

		read := readVersion(t, name, "", 0*time.Second)
		readEtag := verifyNotEmpty(read.Header().Get(headers.ETag), "Read ETag")
		if readEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, readEtag)
		}
		var result bytes.Buffer
		read.Body.WriteTo(&result)

		// If a body exceeds the DataLimitBytes it should be truncated of data, no more
		actual := result.Bytes()
		expected := data[:min(len(data), int(config.DataLimitBytes))]
		if !bytes.Equal(actual, expected) {
			fmt.Printf("[%x] vs [%x]", actual, expected)
			t.Errorf("Expected GET to return firt 1KiB of data that was PUT: actual=[0x%x] vs expected=[0x%x]", actual, expected)
		}
		if uint64(len(result.Bytes())) > config.DataLimitBytes {
			t.Errorf("Should have only read 1KiB no-matter the size of input data")
		}

		// Now test blocking read
		start := time.Now()
		t.Logf("Starting blocking read for name=[%s], version=[%s]", name, writtenEtag)
		blockingRead := readVersion(t, name, writtenEtag, 10*time.Millisecond)
		blockedEtag := verifyNotEmpty(blockingRead.Header().Get(headers.ETag), "Read ETag")
		delta := time.Since(start)
		if blockedEtag != writtenEtag {
			t.Errorf("Expected %s from write but got %s from read", writtenEtag, blockedEtag)
		}
		if delta.Milliseconds() > 100 {
			t.Errorf("Waited %dms which is much longer than 10", delta.Milliseconds())
		}
	})
}

func BenchmarkConcurrentReaders(b *testing.B) {
	config = conf.FromEnv()
	config.JitterPerWatch = 0 * time.Millisecond

	name := "simple"
	resp, err := writeVersion(b, name, "", []byte("test"))
	if err != nil {
		b.Error(err)
	}
	version := resp.Header().Get(headers.ETag)

	b.Run("1:100000 Blocking", func(b *testing.B) {
		var wg sync.WaitGroup
		for range 100000 {
			wg.Add(1)

			go func() {
				defer wg.Done()
				resp = readVersion(b, name, version, 100*time.Millisecond)
				if resp.Header().Get(headers.ETag) != version {
					b.Errorf("Should have gotten written version")
				}
			}()
		}
		wg.Wait()
	})
}

func doConcurrentReadWithWrite(b *testing.B, name string, numReaders int) {
	var wg sync.WaitGroup
	var counter atomic.Int64
	counter.Store(0)
	testDuration := 100 * time.Millisecond

	doWrite := func(i int64) {
		version := fmt.Sprintf("%d", i)
		resp, err := writeVersion(b, name, version, []byte(fmt.Sprintf("test-%d", i)))
		if err != nil {
			b.Error(err)
		}

		hdr := parse.ParseETagToVersion(resp.Header().Get(headers.ETag))
		if hdr != version {
			b.Errorf("Stored version %s should have been the one we gave %s", hdr, version)
		}
		counter.Store(i)
	}
	doWrite(0)

	// Generate writes spaced but not concurrent
	wg.Add(1)
	go func() {
		for i := range 20 {
			doWrite(int64(i + 1))
			time.Sleep(testDuration / 10)
		}
		wg.Done()
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
			resp := readVersion(b, name, fmt.Sprintf("%d", before), 1*time.Second)
			result := parse.ParseETagToVersion(resp.Header().Get(headers.ETag))

			actual, err := strconv.ParseInt(result, 10, 0)
			if actual < 10 && (err != nil || !(int64(actual) > before)) {
				b.Errorf("Got version %d that wasn't > %d, %s", actual, before, resp.Header().Get(headers.ServerTiming))
			}
		}()
	}
	wg.Wait()
}

func BenchmarkConcurrentReadWithWrite(b *testing.B) {
	config = conf.FromEnv()
	config.JitterPerWatch = 0 * time.Millisecond

	testCases := []struct {
		name       string
		numReaders int
	}{
		{"10k", 10000},
		{"50k", 50000},
		{"100k", 100000},
	}

	for _, testCase := range testCases {
		b.Run(testCase.name, func(b *testing.B) {
			doConcurrentReadWithWrite(b, "img", testCase.numReaders)
		})
	}
}

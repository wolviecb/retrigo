package retrigo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	DefaultRetryWaitMin = 1 * time.Microsecond
}

func checkErr(t *testing.T, err error, ok bool) {
	if ok {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
	} else {
		if err == nil {
			t.Fatalf("should fail: %v", err)
		}
	}
}

func TestRequest(t *testing.T) {
	bytesBody := []byte("yo")

	// Fails on invalid request
	_, err := NewRequest("GET", "://foo", nil)
	checkErr(t, err, false)

	// Works with no request body
	_, err = NewRequest("GET", "http://foo", nil)
	checkErr(t, err, true)

	// Should work with multiple urls space separetad
	_, err = NewRequest("GET", "http://foo http://bar", nil)
	checkErr(t, err, true)

	// Works with ReaderFunc request body
	funcBody := ReaderFunc(func() (io.Reader, error) {
		return bytes.NewReader(bytesBody), nil
	})
	_, err = NewRequest("POST", "/", funcBody)
	checkErr(t, err, true)

	// but if ReaderFunc returns error it should fail
	funcBody = ReaderFunc(func() (io.Reader, error) {
		return bytes.NewReader(bytesBody), errors.New("A error")
	})
	_, err = NewRequest("POST", "/", funcBody)
	checkErr(t, err, false)

	// Works with native func error it should fail
	nativeBody := func() (io.Reader, error) {
		return bytes.NewReader(bytesBody), nil
	}
	_, err = NewRequest("POST", "/", nativeBody)
	checkErr(t, err, true)

	// bit if func returns error
	nativeBody = func() (io.Reader, error) {
		return bytes.NewReader(bytesBody), errors.New("A error")
	}
	_, err = NewRequest("POST", "/", nativeBody)
	checkErr(t, err, false)

	// Works with []bytes request body
	_, err = NewRequest("POST", "/", bytesBody)
	checkErr(t, err, true)

	//Works with *bytes.Buffer body
	bufBody := &bytes.Buffer{}
	_, err = NewRequest("GET", "http://foo", bufBody)
	checkErr(t, err, true)

	// Works with io.ReadSeeker
	readSeekerBody := strings.NewReader(string(bytesBody))
	_, err = NewRequest("GET", "http://foo", readSeekerBody)
	checkErr(t, err, true)

	// Works with io.Reader
	readerBody := &custReader{}
	_, err = NewRequest("GET", "http://foo", readerBody)
	checkErr(t, err, true)

	// Works with *bytes.Reader request body
	bytesReaderBody := bytes.NewReader(bytesBody)
	req, err := NewRequest("GET", "/", bytesReaderBody)
	checkErr(t, err, true)

	// Request allows typical HTTP request forming methods
	req.Header.Set("X-Test", "foo")
	if v, ok := req.Header["X-Test"]; !ok || len(v) != 1 || v[0] != "foo" {
		t.Fatalf("bad headers: %v", req.Header)
	}

	// Sets the Content-Length automatically for LenReaders
	if req.ContentLength != 2 {
		t.Fatalf("bad ContentLength: %d", req.ContentLength)
	}

	// Should fail on invalid body types
	req, err = NewRequest("POST", "http://foo", "invalid")
	if err == nil {
		t.Fatalf("Should Fail")
	}
}

func TestFromRequest(t *testing.T) {
	durl := "http://foo"
	// Works with no request body
	httpReq, err := http.NewRequest("GET", durl, nil)
	checkErr(t, err, true)
	_, err = FromRequest(httpReq, durl)
	checkErr(t, err, true)

	// Works with *Reader request body
	durl = "/"
	body := bytes.NewReader([]byte("yo"))
	httpReq, err = http.NewRequest("GET", durl, body)
	checkErr(t, err, true)
	req, err := FromRequest(httpReq, durl)
	checkErr(t, err, true)

	// Preserves headers
	httpReq.Header.Set("X-Test", "foo")
	if v, ok := req.Header["X-Test"]; !ok || len(v) != 1 || v[0] != "foo" {
		t.Fatalf("bad headers: %v", req.Header)
	}

	// Preserves the Content-Length automatically for LenReaders
	if req.ContentLength != 2 {
		t.Fatalf("bad ContentLength: %d", req.ContentLength)
	}
}

// Since normal ways we would generate a Reader have special cases, use a
// custom type here
type custReader struct {
	val string
	pos int
}

func (c *custReader) Read(p []byte) (n int, err error) {
	if c.val == "" {
		c.val = "hello"
	}
	if c.pos >= len(c.val) {
		return 0, io.EOF
	}
	var i int
	for i = 0; i < len(p) && i+c.pos < len(c.val); i++ {
		p[i] = c.val[i+c.pos]
	}
	c.pos += i
	return i, nil
}

func TestClient_Do(t *testing.T) {
	testBytes := []byte("hello")
	// Native func
	testClientDo(t, ReaderFunc(func() (io.Reader, error) {
		return bytes.NewReader(testBytes), nil
	}))
	// Native func, different Go type
	testClientDo(t, func() (io.Reader, error) {
		return bytes.NewReader(testBytes), nil
	})
	// []byte
	testClientDo(t, testBytes)
	// *bytes.Buffer
	testClientDo(t, bytes.NewBuffer(testBytes))
	// *bytes.Reader
	testClientDo(t, bytes.NewReader(testBytes))
	// io.ReadSeeker
	testClientDo(t, strings.NewReader(string(testBytes)))
	// io.Reader
	testClientDo(t, &custReader{})
}

func testClientDo(t *testing.T, body interface{}) {
	// Create a request
	req, err := NewRequest("PUT", "http://127.0.0.1:28934/v1/foo http://127.0.0.2:28934/v1/foo http://127.0.0.3:28934/v1/foo", body)
	checkErr(t, err, true)
	req.Header.Set("foo", "bar")

	// Track the number of times the logging hook was called
	retryCount := -1

	// Track the scheduler index changes
	schedulerCount := 0

	// Create the client. Use short retry windows.
	client := NewClient()
	client.RetryWaitMin = 10 * time.Millisecond
	client.RetryWaitMax = 50 * time.Millisecond
	client.RetryMax = 50
	client.Logger = func(req *Request, mtype, msg string, err error) {
		retryCount++
	}
	client.Scheduler = func(servers []string, j int) (string, int) {
		if j >= len(servers) {
			j = 0
		}
		server := servers[j]
		j++
		schedulerCount++

		return server, j
	}

	// Send the request
	var resp *http.Response
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		var err error
		resp, err = client.Do(req)
		checkErr(t, err, true)
	}()

	select {
	case <-doneCh:
		t.Fatalf("should retry on error")
	case <-time.After(200 * time.Millisecond):
		// Client should still be retrying due to connection failure.
	}

	// Create the mock handler. First we return a 500-range response to ensure
	// that we power through and keep retrying in the face of recoverable
	// errors.
	code := int64(500)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check the request details
		if r.Method != "PUT" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/v1/foo" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}

		// Check the headers
		if v := r.Header.Get("foo"); v != "bar" {
			t.Fatalf("bad header: expect foo=bar, got foo=%v", v)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		checkErr(t, err, true)
		expected := []byte("hello")
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(int(atomic.LoadInt64(&code)))
	})

	// Create a test server
	list, err := net.Listen("tcp", ":28934")
	checkErr(t, err, true)
	defer list.Close()
	go http.Serve(list, handler)

	// Wait again
	select {
	case <-doneCh:
		t.Fatalf("should retry on 500-range")
	case <-time.After(200 * time.Millisecond):
		// Client should still be retrying due to 500's.
	}

	// Start returning 200's
	atomic.StoreInt64(&code, 200)

	// Wait again
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatalf("timed out")
	}

	if resp.StatusCode != 200 {
		t.Fatalf("exected 200, got: %d", resp.StatusCode)
	}

	if retryCount < 0 {
		t.Fatal("request log hook was not called")
	}

	if schedulerCount < 1 {
		t.Fatal("scheduler hook was not called")
	}
}

func TestClient_Do_fails(t *testing.T) {
	// Mock server which always responds 500.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	// Create the client. Use short retry windows so we fail faster.
	client := NewClient()
	client.RetryWaitMin = 10 * time.Millisecond
	client.RetryWaitMax = 10 * time.Millisecond
	client.RetryMax = 2

	// Create the request
	req, err := NewRequest("POST", ts.URL, nil)
	checkErr(t, err, true)

	// Send the request.
	_, err = client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("expected giving up error, got: %#v", err)
	}
}

func TestClient_Get(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the requests.
	resp, err := NewClient().Get(ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().Get("://foo/bar")
	checkErr(t, err, false)
}

func TestClient_GiveUP(t *testing.T) {

	client := NewClient()
	client.RetryWaitMin = 10 * time.Microsecond
	client.RetryWaitMax = 10 * time.Microsecond
	client.RetryMax = 2

	// Should give up and fail if not available
	_, err := client.Get("https://localhost:12365/foo/bar")
	checkErr(t, err, false)

}

func TestClient_Get_clientless(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Get(ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Get_multi(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Get("https://localhost:65535 " + ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Head(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Head(ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
	_, err = NewClient().Head("://foo/bar")
	checkErr(t, err, false)

}

func TestClient_Head_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Head(ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Head_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Head("https://localhost:65535 " + ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Post(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Post(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().Post(
		"://foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, false)
}

func TestClient_Post_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Post(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Post_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Post(
		"https://localhost:65535 "+ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_PostForm(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`hello=world`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Create the form data.
	form, err := url.ParseQuery("hello=world")
	checkErr(t, err, true)

	// Make the request.
	resp, err := NewClient().PostForm(ts.URL+"/foo/bar", form)
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().PostForm("://foo/bar", form)
	checkErr(t, err, false)
}

func TestClient_PostForm_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`hello=world`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Create the form data.
	form, err := url.ParseQuery("hello=world")
	checkErr(t, err, true)

	// Make the request.
	resp, err := PostForm(ts.URL+"/foo/bar", form)
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_PostForm_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`hello=world`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Create the form data.
	form, err := url.ParseQuery("hello=world")
	checkErr(t, err, true)

	// Make the request.
	resp, err := NewClient().PostForm("https://localhost "+ts.URL+"/foo/bar", form)
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Put(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Put(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().Put(
		"://foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, false)
}

func TestClient_Put_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Put(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Put_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Put(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}
func TestClient_Patch(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Patch(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().Patch(
		"://foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, false)
}
func TestClient_Patch_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Patch(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Patch_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Patch(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Delete(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Delete(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()

	_, err = NewClient().Delete(
		"://foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, false)
}

func TestClient_Delete_clientless(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := Delete(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_Delete_multi(t *testing.T) {
	// Mock server which always responds 200.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Fatalf("bad method: %s", r.Method)
		}
		if r.RequestURI != "/foo/bar" {
			t.Fatalf("bad uri: %s", r.RequestURI)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("bad content-type: %s", ct)
		}

		// Check the payload
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("err: %s", err)
		}
		expected := []byte(`{"hello":"world"}`)
		if !bytes.Equal(body, expected) {
			t.Fatalf("bad: %v", body)
		}

		w.WriteHeader(200)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := NewClient().Delete(
		ts.URL+"/foo/bar",
		"application/json",
		strings.NewReader(`{"hello":"world"}`))
	checkErr(t, err, true)
	resp.Body.Close()
}

func TestClient_RequestWithContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("test_200_body"))
	}))
	defer ts.Close()

	req, err := NewRequest(http.MethodGet, ts.URL, nil)
	checkErr(t, err, true)
	ctx, cancel := context.WithCancel(req.Request.Context())
	req = req.WithContext(ctx)

	client := NewClient()

	called := 0
	client.CheckForRetry = func(_ context.Context, resp *http.Response, err error) (bool, error) {
		called++
		return DefaultRetryPolicy(req.Request.Context(), resp, err)
	}

	cancel()
	_, err = client.Do(req)

	if called != 1 {
		t.Fatalf("CheckRetry called %d times, expected 1", called)
	}

	if err != context.Canceled {
		t.Fatalf("Expected context.Canceled err, got: %v", err)
	}
}

func TestClient_CheckRetry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "test_500_body", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := NewClient()

	retryErr := errors.New("retryError")
	called := 0
	client.CheckForRetry = func(_ context.Context, resp *http.Response, err error) (bool, error) {
		if called < 1 {
			called++
			return DefaultRetryPolicy(context.TODO(), resp, err)
		}

		return false, retryErr
	}

	// CheckRetry should return our retryErr value and stop the retry loop.
	_, err := client.Get(ts.URL)

	if called != 1 {
		t.Fatalf("CheckRetry called %d times, expected 1", called)
	}

	if err != retryErr {
		t.Fatalf("Expected retryError, got:%v", err)
	}
}

func TestClient_CheckRetryStop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "test_500_body", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := NewClient()

	// Verify that this stops retries on the first try, with no errors from the client.
	called := 0
	client.CheckForRetry = func(_ context.Context, resp *http.Response, err error) (bool, error) {
		called++
		return false, nil
	}

	_, err := client.Get(ts.URL)

	if called != 1 {
		t.Fatalf("CheckRetry called %d times, expeted 1", called)
	}

	if err != nil {
		t.Fatalf("Expected no error, got:%v", err)
	}
}

func TestBackoff(t *testing.T) {
	type tcase struct {
		min    time.Duration
		max    time.Duration
		i      int
		expect time.Duration
	}
	cases := []tcase{
		{
			time.Second,
			5 * time.Minute,
			0,
			time.Second,
		},
		{
			time.Second,
			5 * time.Minute,
			1,
			2 * time.Second,
		},
		{
			time.Second,
			5 * time.Minute,
			2,
			4 * time.Second,
		},
		{
			time.Second,
			5 * time.Minute,
			3,
			8 * time.Second,
		},
		{
			time.Second,
			5 * time.Minute,
			63,
			5 * time.Minute,
		},
		{
			time.Second,
			5 * time.Minute,
			128,
			5 * time.Minute,
		},
	}

	for _, tc := range cases {
		if v := DefaultBackoff(tc.min, tc.max, tc.i, nil); v != tc.expect {
			t.Fatalf("bad: %#v -> %s", tc, v)
		}
	}
}

func TestJitterBackoff(t *testing.T) {
	type tcase struct {
		min    time.Duration
		max    time.Duration
		i      int
		expect time.Duration
	}
	cases := []tcase{
		// if min >= max LinearJitterBackoff should return min * time.Duration(attemptNum)
		// attemptNum if attempt++ since the counter starts at zero and that would break multiplication
		{
			time.Second,
			time.Second,
			0,
			time.Second * time.Duration(1),
		},
		{
			time.Second,
			time.Second,
			4,
			time.Second * time.Duration(5),
		},
		{
			time.Second,
			time.Second,
			50,
			time.Second * time.Duration(51),
		},
		{
			2 * time.Millisecond,
			time.Millisecond,
			2,
			(2 * time.Millisecond) * time.Duration(3),
		},
		{
			2 * time.Microsecond,
			time.Microsecond,
			2,
			(2 * time.Microsecond) * time.Duration(3),
		},
		// else LinearJitterBackoff will return a number between min and max times attemptNum
		{
			2 * time.Microsecond,
			4 * time.Microsecond,
			2,
			(4 * time.Microsecond) * time.Duration(3),
		},
		{
			2 * time.Millisecond,
			4 * time.Millisecond,
			2,
			(4 * time.Millisecond) * time.Duration(3),
		},
		{
			2 * time.Second,
			4 * time.Second,
			2,
			(4 * time.Second) * time.Duration(3),
		},
		{
			time.Millisecond,
			50 * time.Millisecond,
			2,
			(50 * time.Millisecond) * time.Duration(3),
		},
	}

	for _, tc := range cases {
		if v := LinearJitterBackoff(tc.min, tc.max, tc.i, nil); v > tc.expect {
			t.Fatalf("bad: %#v -> %s", tc, v)
		}
	}
}

func TestClient_BackoffCustom(t *testing.T) {
	var retries int32

	client := NewClient()
	client.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		atomic.AddInt32(&retries, 1)
		return time.Millisecond * 1
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&retries) == int32(client.RetryMax) {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(500)
	}))
	defer ts.Close()

	// Make the request.
	resp, err := client.Get(ts.URL + "/foo/bar")
	checkErr(t, err, true)
	resp.Body.Close()
	if retries != int32(client.RetryMax) {
		t.Fatalf("expected retries: %d != %d", client.RetryMax, retries)
	}
}

// Package retrigo provides a familiar HTTP client interface with
// automatic retries and exponential backoff and multiple target scheduling.
// It is a thin wrapper over the standard net/http client library and exposes
// nearly the same public API. This makes retrigo very easy to drop
// into existing programs.
//
// retrigo performs automatic retries under certain conditions. Mainly, if
// an error is returned by the client (connection errors etc), or if a 500-range
// response is received, then a retry is invoked. Otherwise, the response is
// returned and left to the caller to interpret.
//
// Requests which take a request body should provide a non-nil function
// parameter. The best choice is to provide either a function satisfying
// ReaderFunc which provides multiple io.Readers in an efficient manner, a
// *bytes.Buffer (the underlying raw byte slice will be used) or a raw byte
// slice. As it is a reference type, and we will wrap it as needed by readers,
// we can efficiently re-use the request body without needing to copy it. If an
// io.Reader (such as a *bytes.Reader) is provided, the full body will be read
// prior to the first request, and will be efficiently re-used for any retries.
// ReadSeeker can be used, but some users have observed occasional data races
// between the net/http library and the Seek functionality of some
// implementations of ReadSeeker, so should be avoided if possible.
package retrigo

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/go-cleanhttp"
)

var (
	// DefaultRetryWaitMin is the default minimium wait time
	DefaultRetryWaitMin = 1 * time.Second
	// DefaultRetryWaitMax is the default maximum wait time
	DefaultRetryWaitMax = 30 * time.Second
	// DefaultRetryMax is the default maximum retries
	DefaultRetryMax = 10
	respReadLimit   = int64(4096)
)

// CheckForRetry is called following each request it receives the http.Response
// and error and returns a bool and a error
type CheckForRetry func(ctx context.Context, r *http.Response, err error) (bool, error)

// Client type defines the types used on http.client
type Client struct {
	HTTPClient    *http.Client
	RetryWaitMin  time.Duration
	RetryWaitMax  time.Duration
	RetryMax      int
	CheckForRetry CheckForRetry
	Backoff       Backoff
	Logger        Logger
	Scheduler     Scheduler
}

// Backoff type is the function for calculating the wait time between failed requests
type Backoff func(min, max time.Duration, attempt int, r *http.Response) time.Duration

// Request is the type for storing the http.Request
type Request struct {
	body io.ReadSeeker
	*http.Request
	urls []string
}

type lenReader interface {
	Len() int
}

// WithContext returns wrapped Request with a shallow copy of underlying *http.Request
// with its context changed to ctx. The provided ctx must be non-nil.
func (r *Request) WithContext(ctx context.Context) *Request {
	r.Request = r.Request.WithContext(ctx)
	return r
}

// Logger type is the function for logging error/debug messages
type Logger func(req *Request, mtype, msg string, err error)

// Scheduler type is a the function for that return the next target and index for the Do function
type Scheduler func(servers []string, i int) (string, int)

// DefaultBackoff is the default function for calculating the Backoff period
// it's a simple exponential of 2**attempt * RetryWaitMin limited by RetryWaitMax
func DefaultBackoff(min, max time.Duration, attempt int, r *http.Response) time.Duration {
	m := math.Pow(2, float64(attempt)) * float64(min)
	s := time.Duration(m)
	if float64(s) != m || s > max {
		s = max
	}
	return s
}

// DefaultRetryPolicy is the default policy for retrying http requests
func DefaultRetryPolicy(ctx context.Context, r *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	if err != nil {
		return true, err
	}
	if r.StatusCode == 0 || (r.StatusCode >= 500 && r.StatusCode != 501) {
		return true, nil
	}

	return false, nil
}

// DefaultLogger is a simple default logger
func DefaultLogger(req *Request, mtype, msg string, err error) {
	if err != nil {
		log.Printf(mtype + " " + msg + err.Error())
	} else {
		log.Printf(mtype + " " + msg)
	}
}

// DefaultScheduler round-robins a list of urls from servers
func DefaultScheduler(servers []string, j int) (string, int) {
	// The Do function defines a index j which is used by the Scheduler() for returning the
	// next target and next index, this DefaultScheduler round-robins requests so when j is
	// bigger than the lenght of servers it means that we reached the end of server list and
	// we need to reset and start from the beginning
	if j >= len(servers) {
		j = 0
	}
	server := servers[j]
	j++

	return server, j
}

// NewClient return a default new http.client
func NewClient() *Client {
	return &Client{
		HTTPClient:    cleanhttp.DefaultClient(),
		RetryWaitMin:  DefaultRetryWaitMin,
		RetryWaitMax:  DefaultRetryWaitMax,
		RetryMax:      DefaultRetryMax,
		CheckForRetry: DefaultRetryPolicy,
		Backoff:       DefaultBackoff,
		Logger:        DefaultLogger,
		Scheduler:     DefaultScheduler,
	}
}

// NewRequest create a wrapped request
func NewRequest(method, durl string, body io.ReadSeeker) (*Request, error) {
	var contentLength int64
	raw := body

	if lr, ok := raw.(lenReader); ok {
		contentLength = int64(lr.Len())
	}
	var rBody io.ReadCloser
	if body != nil {
		rBody = ioutil.NopCloser(body)
	}
	dest := strings.Split(durl, " ")
	httpReq, err := http.NewRequest(method, durl, rBody)
	if err != nil {
		return nil, err
	}
	httpReq.ContentLength = contentLength
	return &Request{body, httpReq, dest}, nil
}

// Try to read the response body so we can reuse this connection.
func (c *Client) drainBody(body io.ReadCloser) {
	defer body.Close()
	_, err := io.Copy(ioutil.Discard, io.LimitReader(body, respReadLimit))
	if err != nil {
		mtype := "ERROR"
		msg := "error reading response body"
		c.Logger(nil, mtype, msg, err)
	}
}

// Get is for simple GET requests
func (c *Client) Get(durl string) (*http.Response, error) {
	req, err := NewRequest("GET", durl, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Head is for simple HEAD requests
func (c *Client) Head(durl string) (*http.Response, error) {
	req, err := NewRequest("HEAD", durl, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post is for simple POST requests
func (c *Client) Post(durl, bodyType string, body io.ReadSeeker) (*http.Response, error) {
	req, err := NewRequest("POST", durl, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// Put is for simple PUT requests
func (c *Client) Put(durl, bodyType string, body io.ReadSeeker) (*http.Response, error) {
	req, err := NewRequest("PUT", durl, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// Patch is for simple DELETE requests
func (c *Client) Patch(durl string, bodyType string, body io.ReadSeeker) (*http.Response, error) {
	req, err := NewRequest("PATCH", durl, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// Delete is for simple DELETE requests
func (c *Client) Delete(durl string, bodyType string, body io.ReadSeeker) (*http.Response, error) {
	req, err := NewRequest("DELETE", durl, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// Do wraps calls to the http method with retries
func (c *Client) Do(req *Request) (*http.Response, error) {
	j := 0
	for i := 0; i <= c.RetryMax; i++ {
		var code int

		if req.body != nil {
			if _, err := req.body.Seek(0, 0); err != nil {
				return nil, fmt.Errorf("failed to seek body: %v", err)
			}
		}
		var dest string
		dest, j = c.Scheduler(req.urls, j)
		if durl, err := url.Parse(dest); err == nil {
			req.URL = durl
		} else {
			mtype := "ERROR"
			msg := fmt.Sprintf("Request failed: Bad URL: %s", dest)
			c.Logger(req, mtype, msg, err)
		}
		r, err := c.HTTPClient.Do(req.Request)
		if err != nil {
			mtype := "ERROR"
			msg := fmt.Sprintf("%s %s request failed: ", req.Method, req.URL)
			c.Logger(req, mtype, msg, err)
		}
		if r != nil {
			code = r.StatusCode
		}
		checkOK, checkErr := c.CheckForRetry(req.Context(), r, err)

		if !checkOK {
			if checkErr != nil {
				err = checkErr
			}
			return r, err
		}

		if err == nil {
			c.drainBody(r.Body)
		}

		remain := c.RetryMax - i
		if remain == 0 {
			break
		}
		wait := c.Backoff(c.RetryWaitMin, c.RetryWaitMax, i, r)
		desc := fmt.Sprintf("%s %s", req.Method, req.URL)
		if code > 0 {
			desc = fmt.Sprintf("%s status: %d", desc, code)
		}
		mtype := "DEBUG"
		msg := fmt.Sprintf("%s: retrying in %s (%d left): ", desc, wait, remain)
		c.Logger(req, mtype, msg, err)
		time.Sleep(wait)
	}

	return nil, fmt.Errorf("%s %s giving up after %d attemps", req.Method, req.URL, c.RetryMax+1)
}

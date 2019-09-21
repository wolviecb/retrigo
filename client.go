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
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
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
	// defaultClient is used for performing requests without explicitly making
	// a new client. It is purposely private to avoid modifications.
	defaultClient = NewClient()

	// We need to consume response bodies to maintain http connections, but
	// limit the size we consume to respReadLimit.
	respReadLimit = int64(4096)

	// FirstTarget in the first index that is going to be used when trying
	// to reach the target url from the urls slice
	FirstTarget = 0
)

// CheckForRetry is called following each request, it receives the http.Response
// and error and returns a bool and a error
type CheckForRetry func(ctx context.Context, r *http.Response, err error) (bool, error)

// Client is used to make HTTP requests. It adds additional functionality
// like automatic retries to tolerate minor outages.
type Client struct {
	HTTPClient   *http.Client  // Internal HTTP client.
	RetryWaitMin time.Duration // Minimum time to wait
	RetryWaitMax time.Duration // Maximum time to wait
	RetryMax     int           // Maximum number of retries

	// CheckForRetry specifies the policy for handling retries, and is called
	// after each request. The default policy is DefaultRetryPolicy.
	CheckForRetry CheckForRetry
	Backoff       Backoff // Backoff specifies the policy for how long to wait between retries
	Logger        Logger  // Customer logger instance.

	// Scheduler specifies a the which of the suplied targets should be used next, it's called
	// before each request. The default Scheduler is DefaultScheduler
	Scheduler Scheduler
}

// Backoff specifies a policy for how long to wait between retries.
// It is called after a failing request to determine the amount of time
// that should pass before trying again.
type Backoff func(min, max time.Duration, attempt int, r *http.Response) time.Duration

// ReaderFunc is the type of function that can be given natively to NewRequest
type ReaderFunc func() (io.Reader, error)

// Request wraps the metadata needed to create HTTP requests.
type Request struct {
	body ReaderFunc
	*http.Request
	urls []string
}

// LenReader is an interface implemented by many in-memory io.Reader's. Used
// for automatically sending the right Content-Length header when possible.
type LenReader interface {
	Len() int
}

// WithContext returns wrapped Request with a shallow copy of underlying *http.Request
// with its context changed to ctx. The provided ctx must be non-nil.
func (r *Request) WithContext(ctx context.Context) *Request {
	r.Request = r.Request.WithContext(ctx)
	return r
}

// Logger is for logging error/debug messages
type Logger func(req *Request, mtype, msg string, err error)

// Scheduler is for returning the next target and index for the Do function
type Scheduler func(servers []string, i int) (string, int)

// DefaultBackoff provides a default callback for Client.Backoff which
// will perform exponential backoff based on the attempt number and limited
// by the provided minimum and maximum durations.
func DefaultBackoff(min, max time.Duration, attempt int, r *http.Response) time.Duration {
	m := math.Pow(2, float64(attempt)) * float64(min)
	s := time.Duration(m)
	if float64(s) != m || s > max {
		s = max
	}
	return s
}

// LinearJitterBackoff provides a callback for Client.Backoff which will
// perform linear backoff based on the attempt number and with jitter to
// prevent a thundering herd.
//
// min and max here are *not* absolute values. The number to be multipled by
// the attempt number will be chosen at random from between them, thus they are
// bounding the jitter.
//
// For instance:
// * To get strictly linear backoff of one second increasing each retry, set
// both to one second (1s, 2s, 3s, 4s, ...)
// * To get a small amount of jitter centered around one second increasing each
// retry, set to around one second, such as a min of 800ms and max of 1200ms
// (892ms, 2102ms, 2945ms, 4312ms, ...)
// * To get extreme jitter, set to a very wide spread, such as a min of 100ms
// and a max of 20s (15382ms, 292ms, 51321ms, 35234ms, ...)
func LinearJitterBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	// attemptNum always starts at zero but we want to start at 1 for multiplication
	attemptNum++

	if max <= min {
		// Unclear what to do here, or they are the same, so return min *
		// attemptNum
		return min * time.Duration(attemptNum)
	}

	// Seed rand; doing this every time is fine
	rand := rand.New(rand.NewSource(int64(time.Now().Nanosecond())))

	// Pick a random number that lies somewhere between the min and max and
	// multiply by the attemptNum. attemptNum starts at zero so we always
	// increment here. We first get a random percentage, then apply that to the
	// difference between min and max, and add to min.
	jitter := rand.Float64() * float64(max-min)
	jitterMin := int64(jitter) + int64(min)
	return time.Duration(jitterMin * int64(attemptNum))
}

// DefaultRetryPolicy provides a default callback for Client.CheckRetry, which
// will retry on connection errors and server errors.
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

// DefaultLogger is a simple logger
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

// NewClient creates a new Client with default settings.
func NewClient() *Client {
	return &Client{
		HTTPClient:    cleanhttp.DefaultPooledClient(),
		RetryWaitMin:  DefaultRetryWaitMin,
		RetryWaitMax:  DefaultRetryWaitMax,
		RetryMax:      DefaultRetryMax,
		CheckForRetry: DefaultRetryPolicy,
		Backoff:       DefaultBackoff,
		Logger:        DefaultLogger,
		Scheduler:     DefaultScheduler,
	}
}

func getBodyReaderAndContentLength(rawBody interface{}) (ReaderFunc, int64, error) {
	var bodyReader ReaderFunc
	var contentLength int64

	if rawBody != nil {
		switch body := rawBody.(type) {
		// If they gave us a function already, great! Use it.
		case ReaderFunc:
			bodyReader = body
			tmp, err := body()
			if err != nil {
				return nil, 0, err
			}
			if lr, ok := tmp.(LenReader); ok {
				contentLength = int64(lr.Len())
			}
			if c, ok := tmp.(io.Closer); ok {
				c.Close()
			}

		case func() (io.Reader, error):
			bodyReader = body
			tmp, err := body()
			if err != nil {
				return nil, 0, err
			}
			if lr, ok := tmp.(LenReader); ok {
				contentLength = int64(lr.Len())
			}
			if c, ok := tmp.(io.Closer); ok {
				c.Close()
			}

		// If a regular byte slice, we can read it over and over via new
		// readers
		case []byte:
			buf := body
			bodyReader = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		// If a bytes.Buffer we can read the underlying byte slice over and
		// over
		case *bytes.Buffer:
			buf := body
			bodyReader = func() (io.Reader, error) {
				return bytes.NewReader(buf.Bytes()), nil
			}
			contentLength = int64(buf.Len())

		// We prioritize *bytes.Reader here because we don't really want to
		// deal with it seeking so want it to match here instead of the
		// io.ReadSeeker case.
		case *bytes.Reader:
			buf, err := ioutil.ReadAll(body)
			if err != nil {
				return nil, 0, err
			}
			bodyReader = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		// Compat case
		case io.ReadSeeker:
			raw := body
			bodyReader = func() (io.Reader, error) {
				_, err := raw.Seek(0, 0)
				return ioutil.NopCloser(raw), err
			}
			if lr, ok := raw.(LenReader); ok {
				contentLength = int64(lr.Len())
			}

		// Read all in so we can reset
		case io.Reader:
			buf, err := ioutil.ReadAll(body)
			if err != nil {
				return nil, 0, err
			}
			bodyReader = func() (io.Reader, error) {
				return bytes.NewReader(buf), nil
			}
			contentLength = int64(len(buf))

		default:
			return nil, 0, fmt.Errorf("cannot handle type %T", rawBody)
		}
	}
	return bodyReader, contentLength, nil
}

// FromRequest wraps an http.Request in a retryablehttp.Request
func FromRequest(r *http.Request, durl string) (*Request, error) {
	bodyReader, _, err := getBodyReaderAndContentLength(r.Body)
	if err != nil {
		return nil, err
	}
	dest := strings.Split(durl, " ")
	// Could assert contentLength == r.ContentLength
	return &Request{bodyReader, r, dest}, nil
}

// NewRequest create a wrapped request
func NewRequest(method, durl string, rawBody interface{}) (*Request, error) {
	bodyReader, contentLength, err := getBodyReaderAndContentLength(rawBody)
	if err != nil {
		return nil, err
	}

	// We need to validate all urls on the incoming string before proceding.
	dest := strings.Split(durl, " ")
	for _, t := range dest {
		_, err := url.Parse(t)
		if err != nil {
			return nil, err
		}
	}
	// Then we build the http request with the first url on the resulting slice
	httpReq, err := http.NewRequest(method, dest[0], nil)
	if err != nil {
		return nil, err
	}
	httpReq.ContentLength = contentLength
	return &Request{bodyReader, httpReq, dest}, nil
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

func parseURL(dest string) *url.URL {
	u, _ := url.Parse(dest)
	return u
}

// Do wraps calling an HTTP method with retries.
func (c *Client) Do(req *Request) (*http.Response, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = cleanhttp.DefaultPooledClient()
	}

	j := FirstTarget

	var resp *http.Response
	for i := 0; i <= c.RetryMax; i++ {
		var code int // HTTP response code

		// Always rewind the request body when non-nil.
		if req.body != nil {
			body, err := req.body()
			if err != nil {
				c.HTTPClient.CloseIdleConnections()
				return resp, err
			}
			if c, ok := body.(io.ReadCloser); ok {
				req.Body = c
			} else {
				req.Body = ioutil.NopCloser(body)
			}
		}
		dest := ""
		dest, j = c.Scheduler(req.urls, j)
		req.URL = parseURL(dest)
		// Attempt the request
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

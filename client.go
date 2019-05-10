package retrigo

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
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
	//
	respReadLimit = int64(4096)
)

// CheckForRetry is called following each request it receives the http.Response
// and error and returns a bool and a error
type CheckForRetry func(r *http.Response, err error) (bool, error)

// Client type defines the types used on http.client
type Client struct {
	HTTPClient    *http.Client
	RetryWaitMin  time.Duration
	RetryWaitMax  time.Duration
	RetryMax      int
	CheckForRetry CheckForRetry
	Backoff       Backoff
	Logger        Logger
}

// Backoff type is the function for calculating the wait time between failed requests
type Backoff func(min, max time.Duration, attempt int, r *http.Response) time.Duration

// Request is the type for storing the http.Request
type Request struct {
	body io.ReadSeeker
	*http.Request
}

// Logger type is the function for logging error/debug messages
type Logger func(mtype, msg string, err error)

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
func DefaultRetryPolicy(r *http.Response, err error) (bool, error) {
	if err != nil {
		return true, err
	}
	if r.StatusCode == http.StatusInternalServerError {
		return true, nil
	}

	return false, nil
}

// DefaultLogger is a simple default logger
func DefaultLogger(mtype, msg string, err error) {
	log.Printf(mtype + " " + msg + err.Error())
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
	}
}

// NewRequest create a wrapped request
func NewRequest(method, url string, body io.ReadSeeker) (*Request, error) {
	var rBody io.ReadCloser
	if body != nil {
		rBody = ioutil.NopCloser(body)
	}
	httpReq, err := http.NewRequest(method, url, rBody)
	if err != nil {
		return nil, err
	}
	return &Request{body, httpReq}, nil
}

// Try to read the response body so we can reuse this connection.
func (c *Client) drainBody(body io.ReadCloser) {
	defer body.Close()
	_, err := io.Copy(ioutil.Discard, io.LimitReader(body, respReadLimit))
	if err != nil {
		mtype := "ERROR"
		msg := "error reading response body"
		c.Logger(mtype, msg, err)
	}
}

// Get is for simple GET requests
func (c *Client) Get(url string) (*http.Response, error) {
	req, err := NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// Post is for simple POST requests
func (c *Client) Post(url, bodyType string, body io.ReadSeeker) (*http.Response, error) {
	req, err := NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", bodyType)
	return c.Do(req)
}

// Do wraps calls to the http method with retries
func (c *Client) Do(req *Request) (*http.Response, error) {
	for i := 0; i < c.RetryMax; i++ {
		var code int

		if req.body != nil {
			if _, err := req.body.Seek(0, 0); err != nil {
				return nil, fmt.Errorf("failed to seek body: %v", err)
			}
		}

		r, err := c.HTTPClient.Do(req.Request)
		if err != nil {
			mtype := "ERROR"
			msg := fmt.Sprintf("%s %s request failed: ", req.Method, req.URL)
			c.Logger(mtype, msg, err)
		}
		if r != nil {
			code = r.StatusCode
		}
		checkOK, checkErr := c.CheckForRetry(r, err)

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
		c.Logger(mtype, msg, err)
		time.Sleep(wait)
	}

	return nil, fmt.Errorf("%s %s giving up after %d attemps", req.Method, req.URL, c.RetryMax+1)
}

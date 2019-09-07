# Retrigo - Very simple http retriable client

[![License: MPL 2.0](https://img.shields.io/badge/License-MPL%202.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0) [![Build Status](https://travis-ci.com/wolviecb/retrigo.svg?branch=master)](https://travis-ci.com/wolviecb/retrigo)

Retrigo is a very striped down version of the [hashicorp/go-retryablehttp](https://github.com/hashicorp/go-retryablehttp), it implements the familiar HTTP package with automatic retries and backoff method, check [hashicorp/go-retryablehttp](https://github.com/hashicorp/go-retryablehttp) for more details

## Usage

Using default client

```go
c := retrigo.NewClient()
resp, err := c.Post("http://localhost", "text/plain", bytes.NewReader([]byte{}))
```

Setting some parameters

```go
c := retrigo.Client{
  HTTPClient: &http.Client{
    Timeout: 10 * time.Second,
  },
  Logger:        retrigo.DefaulLogger,
  RetryWaitMin:  20 * time.Millisecond,
  RetryWaitMax:  10 * time.Second,
  RetryMax:      5,
  CheckForRetry: retrigo.DefaultRetryPolicy,
  Backoff:       retrigo.DefaultBackoff,
}
```

The url parameter (`c.Post("URL")`) can be one URL or a space separated list of URLs that the library will randomly choose as target (e. g. `"URL1 URL2 URL3"`)

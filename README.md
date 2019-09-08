# Retrigo - Very simple http retriable client

[![License: MPL 2.0](https://img.shields.io/badge/License-MPL%202.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0) [![Build Status](https://travis-ci.com/wolviecb/retrigo.svg?branch=master)](https://travis-ci.com/wolviecb/retrigo)

Retrigo is a very striped down version of the [hashicorp/go-retryablehttp](https://github.com/hashicorp/go-retryablehttp), it implements the familiar HTTP package with automatic retries and backoff method, check [hashicorp/go-retryablehttp](https://github.com/hashicorp/go-retryablehttp) for more details

## Usage

Using default client

```go
c := retrigo.NewClient()
resp, err := c.Post("http://localhost", "text/plain", bytes.NewReader([]byte{}))
```

Setting parameters

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
  Scheduler: retrigo.DefaulScheduler,
}
resp, err := c.Get("http://localhost")
```

## Multiple targets

The url parameter (`c.Post("URL")`) can be one url or a space separated list of urls that the library will choose as target (e. g. `"URL1 URL2 URL3"`). The default Scheduler() will round-robin around all urls of the list, you can implement other scheduling strategies by defining your own Scheduler() e.g.:

```go
c := retrigo.NewClient()
c = func(servers []string, j int) (string, int) {
  ...
```

## Logging

The Logger() function defines the logging methods/format, this function will recieve a severity, a message and a error struct.

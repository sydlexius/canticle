package petitlyrics

// NewClientForTest points a client at a test server. It lives in an _test.go
// file, so only this package's tests (including the external petitlyrics_test
// package, which drives the client through a real orchestrator lane) can call it.
func NewClientForTest(baseURL string) *Client {
	c := NewClient()
	c.baseURL = baseURL
	return c
}

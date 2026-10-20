package workbuddy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
)

// doJSON performs an upstream JSON request with a bounded response body.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte, limit int64) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	} else {
		r = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(b)) > limit {
		return nil, resp.StatusCode, errors.New("upstream response too large")
	}
	return b, resp.StatusCode, nil
}

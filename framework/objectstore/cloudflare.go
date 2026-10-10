package objectstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CloudflareObjectStore uses the Container's private R2 outbound binding.
// No account-wide S3 credential or public object endpoint is needed.
type CloudflareObjectStore struct {
	endpoint, token, bucket string
	compress                bool
	client                  *http.Client
}

const cloudflareObjectLimit = 100 * 1024 * 1024

func NewCloudflareObjectStore(cfg *Config) (*CloudflareObjectStore, error) {
	endpoint := strings.TrimRight(os.Getenv("BIFROST_CLOUDFLARE_R2_URL"), "/")
	token := os.Getenv("BIFROST_CLOUDFLARE_R2_TOKEN")
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "http" || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("private Cloudflare R2 endpoint required")
	}
	ip := net.ParseIP(target.Hostname())
	if target.Hostname() != "bifrost.objects" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("Cloudflare R2 endpoint must be the private binding or loopback")
	}
	if _, err = hex.DecodeString(token); err != nil || len(token) != 64 || cfg.Bucket.GetValue() == "" {
		return nil, errors.New("Cloudflare R2 capability and bucket required")
	}
	return &CloudflareObjectStore{endpoint: endpoint, token: token, bucket: cfg.Bucket.GetValue(), compress: cfg.Compress, client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (s *CloudflareObjectStore) request(ctx context.Context, method, path string, body []byte, headers map[string]string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Bifrost-R2-Capability", s.token)
	request.Header.Set("X-Bifrost-R2-Bucket", s.bucket)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, errors.New("Cloudflare R2 request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("Cloudflare R2 rejected request: HTTP %d", response.StatusCode)
	}
	return response, nil
}
func (s *CloudflareObjectStore) Put(ctx context.Context, key string, data []byte, tags map[string]string) error {
	if key == "" {
		return errors.New("object key required")
	}
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if s.compress {
		var err error
		data, err = gzipCompress(data)
		if err != nil {
			return err
		}
		headers["X-Object-Encoding"] = "gzip"
	}
	if len(data) > cloudflareObjectLimit {
		return errors.New("Cloudflare R2 object exceeds Worker upload limit")
	}
	if len(tags) > 0 {
		encoded, err := json.Marshal(tags)
		if err != nil {
			return err
		}
		headers["X-Object-Tags"] = string(encoded)
	}
	response, err := s.request(ctx, http.MethodPut, "/object?key="+url.QueryEscape(key), data, headers)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
func (s *CloudflareObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	response, err := s.request(ctx, http.MethodGet, "/object?key="+url.QueryEscape(key), nil, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, cloudflareObjectLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > cloudflareObjectLimit {
		return nil, errors.New("Cloudflare R2 object too large")
	}
	if response.Header.Get("X-Object-Encoding") == "gzip" {
		return gzipDecompress(data)
	}
	return data, nil
}
func (s *CloudflareObjectStore) Delete(ctx context.Context, key string) error {
	response, err := s.request(ctx, http.MethodDelete, "/object?key="+url.QueryEscape(key), nil, nil)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
func (s *CloudflareObjectStore) DeleteBatch(ctx context.Context, keys []string) error {
	for start := 0; start < len(keys); start += 1000 {
		body, err := json.Marshal(keys[start:min(start+1000, len(keys))])
		if err != nil {
			return err
		}
		response, err := s.request(ctx, http.MethodPost, "/delete", body, map[string]string{"Content-Type": "application/json"})
		if err != nil {
			return err
		}
		if err = response.Body.Close(); err != nil {
			return err
		}
	}
	return nil
}
func (s *CloudflareObjectStore) ListByPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	cursor := ""
	for {
		response, err := s.request(ctx, http.MethodGet, "/list?"+url.Values{"prefix": {prefix}, "cursor": {cursor}}.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Objects   []ObjectInfo `json:"objects"`
			Cursor    string       `json:"cursor"`
			Truncated bool         `json:"truncated"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&page)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		objects = append(objects, page.Objects...)
		if !page.Truncated {
			return objects, nil
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return nil, errors.New("invalid R2 pagination cursor")
		}
		cursor = page.Cursor
	}
}
func (s *CloudflareObjectStore) Ping(ctx context.Context) error {
	response, err := s.request(ctx, http.MethodHead, "/health", nil, nil)
	if err != nil {
		return err
	}
	return response.Body.Close()
}
func (s *CloudflareObjectStore) Close() error { s.client.CloseIdleConnections(); return nil }

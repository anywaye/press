package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CFStore is a Cloudflare REST API-backed store. It implements Client using
// the Cloudflare R2 REST API with a Bearer token, avoiding S3 signing.
type CFStore struct {
	accountID string
	bucket    string
	token     string
	client    *http.Client
}

func NewCFStore(accountID, bucket, token string) (*CFStore, error) {
	if accountID == "" || bucket == "" || token == "" {
		return nil, fmt.Errorf("store: account, bucket and token are required")
	}
	return &CFStore{
		accountID: accountID,
		bucket:    bucket,
		token:     token,
		client:    &http.Client{},
	}, nil
}

func (c *CFStore) base() string {
	return fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/r2/buckets/%s", c.accountID, c.bucket)
}

func (c *CFStore) Put(ctx context.Context, key string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base()+"/objects/"+key, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("store: PUT %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("store: PUT %s: status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (c *CFStore) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/objects/"+key, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: GET %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("store: GET %s: status %d", key, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *CFStore) Head(ctx context.Context, key string) (int64, error) {
	data, err := c.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (c *CFStore) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	cursor := ""
	for {
		u := c.base() + "/objects?prefix=" + url.QueryEscape(prefix)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("store: LIST %s: %w", prefix, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("store: LIST %s: status %d: %s", prefix, resp.StatusCode, string(body))
		}
		var res struct {
			Result []struct {
				Key string `json:"key"`
			} `json:"result"`
			ResultInfo struct {
				Cursor      string `json:"cursor"`
				IsTruncated bool   `json:"is_truncated"`
			} `json:"result_info"`
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("store: LIST %s: parse: %w", prefix, err)
		}
		for _, o := range res.Result {
			keys = append(keys, o.Key)
		}
		if !res.ResultInfo.IsTruncated || res.ResultInfo.Cursor == "" {
			break
		}
		cursor = res.ResultInfo.Cursor
	}
	return keys, nil
}

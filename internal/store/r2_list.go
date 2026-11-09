package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

func (r *R2) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token string
	for {
		u := fmt.Sprintf("%s/%s?list-type=2&prefix=%s", r.endpoint, r.bucket, url.QueryEscape(prefix))
		if token != "" {
			u += "&continuation-token=" + url.QueryEscape(token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		empty := sha256.Sum256(nil)
		req.Header.Set("x-amz-content-sha256", hex.EncodeToString(empty[:]))
		r.sign(req, empty[:], time.Now().UTC())
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("store: LIST %s: %w", prefix, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("store: LIST %s: status %d", prefix, resp.StatusCode)
		}
		var res listResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("store: LIST %s: parse: %w", prefix, err)
		}
		for _, c := range res.Contents {
			keys = append(keys, c.Key)
		}
		if !res.IsTruncated {
			break
		}
		token = res.NextContinuationToken
		if token == "" {
			break
		}
	}
	return keys, nil
}

type listResult struct {
	XMLName               xml.Name     `xml:"ListBucketResult"`
	Contents              []listObject `xml:"Contents"`
	IsTruncated           bool         `xml:"IsTruncated"`
	NextContinuationToken string       `xml:"NextContinuationToken"`
}

type listObject struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
}

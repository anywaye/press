package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Client is the S3 API surface press uses: put an object, get an object back,
// ask an object's size. The rest of the S3 API is absent because press does
// not use it, and an interface wider than its uses is a wider test surface
// for no behavior.
type Client interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Head(ctx context.Context, key string) (int64, error)
}

// R2 talks to an R2 bucket over its S3 API. Zero value is not usable; build
// with NewR2.
type R2 struct {
	endpoint string
	bucket   string
	region   string
	keyID    string
	secret   string
	sessionToken string
	client       *http.Client
}

// NewR2 builds a client. The endpoint and bucket are required because every
// other field has a safe default and these two do not: an empty endpoint
// silently aims at AWS, which is not where the objects live.
func NewR2(endpoint, bucket, accessKeyID, secretAccessKey string) (*R2, error) {
	return NewR2WithToken(endpoint, bucket, accessKeyID, secretAccessKey, "")
}

// NewR2WithToken builds a client whose credentials carry a session token —
// the shape of R2's temporary credentials, derived from a parent R2 API
// token. A SigV4 signature without the token header answers 403 even though
// the key pair is valid.
func NewR2WithToken(endpoint, bucket, accessKeyID, secretAccessKey, sessionToken string) (*R2, error) {
	if endpoint == "" || bucket == "" {
		return nil, fmt.Errorf("store: endpoint and bucket are required (got endpoint=%q bucket=%q)", endpoint, bucket)
	}
	return &R2{
		endpoint:     strings.TrimSuffix(endpoint, "/"),
		bucket:       bucket,
		region:       "auto",
		keyID:        accessKeyID,
		secret:       secretAccessKey,
		sessionToken: sessionToken,
		client:       &http.Client{Timeout: 120 * time.Second},
	}, nil
}

func (r *R2) url(key string) string {
	return fmt.Sprintf("%s/%s/%s", r.endpoint, r.bucket, key)
}

// Put writes the whole object. press's objects are tarballs and built pages;
// a streaming multipart upload is a second implementation of this method for
// a size press never reaches.
func (r *R2) Put(ctx context.Context, key string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.url(key), bytes.NewReader(body))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sum[:]))
	r.sign(req, sum[:], time.Now().UTC())
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("store: PUT %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return fmt.Errorf("store: PUT %s: status %d: %s", key, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// Get reads the whole object back.
func (r *R2) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(key), nil)
	if err != nil {
		return nil, err
	}
	empty := sha256.Sum256(nil)
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(empty[:]))
	r.sign(req, empty[:], time.Now().UTC())
	resp, err := r.client.Do(req)
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

// Head reports the object's size.
func (r *R2) Head(ctx context.Context, key string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.url(key), nil)
	if err != nil {
		return 0, err
	}
	empty := sha256.Sum256(nil)
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(empty[:]))
	r.sign(req, empty[:], time.Now().UTC())
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("store: HEAD %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("store: HEAD %s: status %d", key, resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// ErrNotFound is the one storage error the ingest path has to branch on.
var ErrNotFound = fmt.Errorf("store: object not found")

func (r *R2) sign(req *http.Request, payloadHash []byte, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)

	host := req.URL.Host
	req.Header.Set("host", host)

	type hv struct{ k, v string }
	headers := []hv{
		{"host", host},
		{"x-amz-content-sha256", req.Header.Get("x-amz-content-sha256")},
		{"x-amz-date", amzDate},
	}
	if r.sessionToken != "" {
		req.Header.Set("x-amz-security-token", r.sessionToken)
		headers = append(headers, hv{"x-amz-security-token", r.sessionToken})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].k < headers[j].k })

	var signedHeadersList, canonicalHeaders strings.Builder
	for i, h := range headers {
		if i > 0 {
			signedHeadersList.WriteByte(';')
		}
		signedHeadersList.WriteString(h.k)
		canonicalHeaders.WriteString(h.k)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(h.v)
		canonicalHeaders.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeadersList.String(),
		hex.EncodeToString(payloadHash),
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, r.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+r.secret), dateStamp)
	kRegion := hmacSHA256(kDate, r.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		r.keyID, scope, signedHeadersList.String(), signature,
	))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

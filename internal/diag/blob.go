package diag

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
)

// BlobAPIVersion is the x-ms-version of the Blob Storage REST calls.
const BlobAPIVersion = "2023-11-03"

// BlobStore keeps bundles in an Azure Blob Storage container through the
// REST API, authenticated with a Microsoft Entra ID bearer token for
// https://storage.azure.com/ (the hub's managed identity needs "Storage
// Blob Data Contributor" on the container). No account keys, no SAS.
type BlobStore struct {
	// Container is https://<account>.blob.core.windows.net/<container>.
	Container string
	Tokens    azauth.TokenSource
	Client    *http.Client
	// ContentType of the blobs Put writes (default application/zip).
	ContentType string
}

// BlobError is a failed Blob Storage call.
type BlobError struct {
	Op     string
	Status int
	Code   string // x-ms-error-code
	Msg    string
}

func (e *BlobError) Error() string {
	s := fmt.Sprintf("blob storage %s: %d", e.Op, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		s += " (the hub's identity needs the role Storage Blob Data Contributor on the container)"
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// Is maps 404 to ErrNotFound.
func (e *BlobError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// Location implements Store.
func (s *BlobStore) Location() string { return s.Container }

func (s *BlobStore) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (s *BlobStore) blobURL(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return s.Container + "/" + strings.Join(parts, "/")
}

func (s *BlobStore) do(ctx context.Context, op, method, u string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	tok, err := s.Tokens.Token(ctx, azauth.ResourceStorage)
	if err != nil {
		return nil, fmt.Errorf("blob storage %s: token: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("x-ms-version", BlobAPIVersion)
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob storage %s: %w", op, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		e := &BlobError{Op: op, Status: resp.StatusCode, Code: resp.Header.Get("x-ms-error-code")}
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(b, &x) == nil {
			if e.Code == "" {
				e.Code = x.Code
			}
			e.Msg = strings.SplitN(strings.TrimSpace(x.Message), "\n", 2)[0]
		}
		return nil, e
	}
	return resp, nil
}

// Put implements Store (Put Blob, a block blob in one request; bundles
// are far below its 5000 MiB limit).
func (s *BlobStore) Put(ctx context.Context, name string, r io.Reader, size int64, meta map[string]string) error {
	h := http.Header{}
	h.Set("x-ms-blob-type", "BlockBlob")
	ct := s.ContentType
	if ct == "" {
		ct = "application/zip"
	}
	h.Set("Content-Type", ct)
	for k, v := range meta {
		h.Set("x-ms-meta-"+k, v)
	}
	resp, err := s.do(ctx, "put", http.MethodPut, s.blobURL(name), r, size, h)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// Get implements Store (Get Blob).
func (s *BlobStore) Get(ctx context.Context, name string) (io.ReadCloser, BlobInfo, error) {
	resp, err := s.do(ctx, "get", http.MethodGet, s.blobURL(name), nil, 0, nil)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	bi := BlobInfo{Name: name, Size: resp.ContentLength, Meta: map[string]string{}}
	bi.Modified, _ = http.ParseTime(resp.Header.Get("Last-Modified"))
	for k, v := range resp.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-meta-") && len(v) > 0 {
			bi.Meta[strings.TrimPrefix(lk, "x-ms-meta-")] = v[0]
		}
	}
	return resp.Body, bi, nil
}

// listResult is the List Blobs answer.
type listResult struct {
	Blobs []struct {
		Name       string `xml:"Name"`
		Properties struct {
			LastModified  string `xml:"Last-Modified"`
			ContentLength string `xml:"Content-Length"`
		} `xml:"Properties"`
		Metadata struct {
			Items []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"Metadata"`
	} `xml:"Blobs>Blob"`
	NextMarker string `xml:"NextMarker"`
}

// List implements Store (List Blobs with include=metadata, all pages).
func (s *BlobStore) List(ctx context.Context, prefix string) ([]BlobInfo, error) {
	var out []BlobInfo
	marker := ""
	for page := 0; page < 100; page++ {
		q := url.Values{"restype": {"container"}, "comp": {"list"}, "include": {"metadata"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := s.do(ctx, "list", http.MethodGet, s.Container+"?"+q.Encode(), nil, 0, nil)
		if err != nil {
			return nil, err
		}
		var lr listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&lr)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("blob storage list: %w", err)
		}
		for _, b := range lr.Blobs {
			bi := BlobInfo{Name: b.Name, Meta: map[string]string{}}
			bi.Size, _ = strconv.ParseInt(b.Properties.ContentLength, 10, 64)
			bi.Modified, _ = http.ParseTime(b.Properties.LastModified)
			for _, it := range b.Metadata.Items {
				bi.Meta[strings.ToLower(it.XMLName.Local)] = it.Value
			}
			out = append(out, bi)
		}
		if lr.NextMarker == "" {
			return out, nil
		}
		marker = lr.NextMarker
	}
	return out, errors.New("blob storage list: too many pages")
}

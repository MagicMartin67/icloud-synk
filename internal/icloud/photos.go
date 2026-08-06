package icloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// The photo library lives in a CloudKit private database. Assets come in pairs:
// a CPLMaster record holding the original file, and a CPLAsset record holding
// the user-visible metadata (capture date, orientation, edits).
const (
	photosZone       = "PrimarySync"
	photosDatabase   = "/database/1/com.apple.photos.cloud/production/private"
	photosRecordType = "CPLAssetAndMasterByAssetDateWithoutHiddenOrDeleted"
)

// Photo is one asset ready to be written to disk.
type Photo struct {
	// ID is the master record name: stable, unique, and used as the state key.
	ID       string
	Filename string
	Size     int64
	Checksum string
	Created  time.Time
	Added    time.Time
	URL      string
	IsVideo  bool
}

// SafeFilename returns a filename that is safe on Linux, prefixed with the ID
// when the original name is missing.
func (p Photo) SafeFilename() string {
	name := strings.TrimSpace(p.Filename)
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimPrefix(name, ".")
	if name == "" {
		name = p.ID + ".bin"
	}
	return name
}

func (c *Client) photosRoot() (string, error) {
	if u, err := c.service("ckdatabasews"); err == nil {
		return u, nil
	}
	return c.service("photos")
}

// ckValue is a CloudKit field value; the useful part is buried in "value".
type ckValue struct {
	Value json.RawMessage `json:"value"`
	Type  string          `json:"type"`
}

func (v ckValue) str() string {
	var s string
	if json.Unmarshal(v.Value, &s) == nil {
		return s
	}
	return ""
}

func (v ckValue) int64() int64 {
	var n int64
	if json.Unmarshal(v.Value, &n) == nil {
		return n
	}
	return 0
}

// asset is the nested object CloudKit uses for file resources.
func (v ckValue) asset() (downloadURL string, size int64, checksum string) {
	var a struct {
		DownloadURL  string `json:"downloadURL"`
		Size         int64  `json:"size"`
		FileChecksum string `json:"fileChecksum"`
	}
	if json.Unmarshal(v.Value, &a) == nil {
		return a.DownloadURL, a.Size, a.FileChecksum
	}
	return "", 0, ""
}

func (v ckValue) recordName() string {
	var r struct {
		RecordName string `json:"recordName"`
	}
	if json.Unmarshal(v.Value, &r) == nil {
		return r.RecordName
	}
	return ""
}

type ckRecord struct {
	RecordName string             `json:"recordName"`
	RecordType string             `json:"recordType"`
	Fields     map[string]ckValue `json:"fields"`
	Created    struct {
		Timestamp int64 `json:"timestamp"`
	} `json:"created"`
}

// photosDesiredKeys is the minimum set of fields we need: the original
// resource, its name and type, plus the capture date from the asset record.
var photosDesiredKeys = []string{
	"resOriginalRes", "resOriginalFileType", "resOriginalFingerprint",
	"resVidFullRes", "resVidFullFileType",
	"filenameEnc", "originalOrientation", "itemType",
	"masterRef", "assetDate", "addedDate", "isDeleted", "isHidden",
}

// ListPhotos enumerates the whole library, invoking fn for each asset in
// ascending capture-date order. Iteration stops if fn returns an error.
func (c *Client) ListPhotos(ctx context.Context, pageSize int, fn func(Photo) error) error {
	root, err := c.photosRoot()
	if err != nil {
		return err
	}
	if pageSize <= 0 {
		pageSize = 200
	}
	endpoint := root + photosDatabase + "/records/query"
	params := url.Values{
		"remapEnums":          {"True"},
		"getCurrentSyncToken": {"True"},
	}

	startRank := 0
	for {
		payload := map[string]any{
			"query": map[string]any{
				"recordType": photosRecordType,
				"filterBy": []map[string]any{
					{
						"fieldName":  "startRank",
						"fieldValue": map[string]any{"type": "INT64", "value": startRank},
						"comparator": "EQUALS",
					},
					{
						"fieldName":  "direction",
						"fieldValue": map[string]any{"type": "STRING", "value": "ASCENDING"},
						"comparator": "EQUALS",
					},
				},
			},
			// Each asset yields two records (master + asset).
			"resultsLimit": pageSize * 2,
			"desiredKeys":  photosDesiredKeys,
			"zoneID":       map[string]any{"zoneName": photosZone},
		}

		var resp struct {
			Records []ckRecord `json:"records"`
		}
		if err := c.postJSON(ctx, endpoint, params, payload, &resp); err != nil {
			return err
		}
		if len(resp.Records) == 0 {
			return nil
		}

		masters := make([]ckRecord, 0, len(resp.Records)/2+1)
		assets := make(map[string]ckRecord, len(resp.Records)/2+1)
		for _, r := range resp.Records {
			switch r.RecordType {
			case "CPLMaster":
				masters = append(masters, r)
			case "CPLAsset":
				if id := r.Fields["masterRef"].recordName(); id != "" {
					assets[id] = r
				}
			}
		}
		if len(masters) == 0 {
			return nil
		}

		for _, m := range masters {
			p, ok := buildPhoto(m, assets[m.RecordName])
			if !ok {
				continue
			}
			if err := fn(p); err != nil {
				return err
			}
		}
		startRank += len(masters)
	}
}

func buildPhoto(master, asset ckRecord) (Photo, bool) {
	if asset.Fields["isDeleted"].int64() == 1 {
		return Photo{}, false
	}
	dlURL, size, checksum := master.Fields["resOriginalRes"].asset()
	if dlURL == "" {
		// Some videos only expose the full-resolution video resource.
		dlURL, size, checksum = master.Fields["resVidFullRes"].asset()
	}
	if dlURL == "" {
		return Photo{}, false
	}

	name := master.RecordName
	if enc := master.Fields["filenameEnc"].str(); enc != "" {
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil && len(raw) > 0 {
			name = string(raw)
		}
	}

	created := cloudKitTime(asset.Fields["assetDate"].int64())
	if created.IsZero() {
		created = cloudKitTime(master.Created.Timestamp)
	}

	fileType := master.Fields["resOriginalFileType"].str()
	isVideo := strings.HasPrefix(fileType, "public.") && strings.Contains(fileType, "movie")
	if ext := strings.ToLower(path.Ext(name)); ext == ".mov" || ext == ".mp4" || ext == ".m4v" {
		isVideo = true
	}

	return Photo{
		ID:       master.RecordName,
		Filename: name,
		Size:     size,
		Checksum: checksum,
		Created:  created,
		Added:    cloudKitTime(asset.Fields["addedDate"].int64()),
		URL:      dlURL,
		IsVideo:  isVideo,
	}, true
}

// cloudKitTime converts CloudKit's millisecond timestamps.
func cloudKitTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// DownloadPhoto streams an asset into w. The URL is pre-signed, so no session
// headers are required, but it expires within minutes of being listed.
func (c *Client) DownloadPhoto(ctx context.Context, p Photo, w io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, &APIError{Status: resp.StatusCode, URL: p.URL, Body: string(body)}
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, err
	}
	if p.Size > 0 && n != p.Size {
		return n, fmt.Errorf("icloud: %s truncated: got %d bytes, expected %d", p.SafeFilename(), n, p.Size)
	}
	return n, nil
}

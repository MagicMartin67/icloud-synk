package icloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// RootFolderID is the drivewsid of the iCloud Drive root.
const RootFolderID = "FOLDER::com.apple.CloudDocs::root"

// DefaultZone is the document zone of iCloud Drive itself. App libraries
// (Pages, Numbers, …) live in their own zones.
const DefaultZone = "com.apple.CloudDocs"

// Item types returned by the Drive API.
const (
	TypeFolder     = "FOLDER"
	TypeFile       = "FILE"
	TypeAppLibrary = "APP_LIBRARY"
	TypePackage    = "PACKAGE"
)

// Item is one node of the Drive hierarchy.
type Item struct {
	DrivewsID string `json:"drivewsid"`
	DocwsID   string `json:"docwsid"`
	Zone      string `json:"zone"`
	Name      string `json:"name"`
	Extension string `json:"extension"`
	Etag      string `json:"etag"`
	Type      string `json:"type"`
	ParentID  string `json:"parentId"`
	Size      int64  `json:"size"`

	DateCreated  time.Time `json:"dateCreated"`
	DateModified time.Time `json:"dateModified"`
	DateChanged  time.Time `json:"dateChanged"`

	// Present on FILE items that are really bundles (.pages, .key …).
	ItemCount int `json:"itemCount"`
}

// FullName is the file name as it should appear on disk. The API keeps the
// extension in a separate field.
func (i Item) FullName() string {
	if i.Extension == "" {
		return i.Name
	}
	return i.Name + "." + i.Extension
}

// IsDir reports whether the item should become a directory locally.
func (i Item) IsDir() bool {
	return i.Type == TypeFolder || i.Type == TypeAppLibrary
}

// ModTime is the best modification time the API offers.
func (i Item) ModTime() time.Time {
	if !i.DateModified.IsZero() {
		return i.DateModified
	}
	if !i.DateChanged.IsZero() {
		return i.DateChanged
	}
	return i.DateCreated
}

// Folder is a folder plus its direct children.
type Folder struct {
	Item
	Items []Item `json:"items"`
}

// driveRoot returns the drivews service URL.
func (c *Client) driveRoot() (string, error) { return c.service("drivews") }

// docsRoot returns the docws service URL.
func (c *Client) docsRoot() (string, error) { return c.service("docws") }

// ListFolders fetches the children of several folders in one request. Passing
// many ids at once is what keeps polling cheap.
func (c *Client) ListFolders(ctx context.Context, drivewsIDs []string) ([]Folder, error) {
	if len(drivewsIDs) == 0 {
		return nil, nil
	}
	root, err := c.driveRoot()
	if err != nil {
		return nil, err
	}
	payload := make([]map[string]any, 0, len(drivewsIDs))
	for _, id := range drivewsIDs {
		payload = append(payload, map[string]any{
			"drivewsid":   id,
			"partialData": false,
		})
	}
	var folders []Folder
	if err := c.postJSON(ctx, root+"/retrieveItemDetailsInFolders", nil, payload, &folders); err != nil {
		return nil, err
	}
	// The trash is exposed as a child of the root; never mirror it.
	for i := range folders {
		kept := folders[i].Items[:0]
		for _, it := range folders[i].Items {
			if strings.Contains(it.DrivewsID, "TRASH_ROOT") {
				continue
			}
			kept = append(kept, it)
		}
		folders[i].Items = kept
	}
	return folders, nil
}

// DownloadURL resolves the short-lived direct URL of a file's contents.
func (c *Client) DownloadURL(ctx context.Context, item Item) (string, error) {
	docs, err := c.docsRoot()
	if err != nil {
		return "", err
	}
	zone := item.Zone
	if zone == "" {
		zone = DefaultZone
	}
	var resp struct {
		DataToken struct {
			URL string `json:"url"`
		} `json:"data_token"`
		PackageToken struct {
			URL string `json:"url"`
		} `json:"package_token"`
	}
	params := url.Values{"document_id": {item.DocwsID}}
	endpoint := docs + "/ws/" + zone + "/download/by_id"
	if err := c.serviceJSON(ctx, http.MethodGet, endpoint, params, nil, &resp); err != nil {
		return "", err
	}
	if resp.DataToken.URL != "" {
		return resp.DataToken.URL, nil
	}
	if resp.PackageToken.URL != "" {
		return resp.PackageToken.URL, nil
	}
	return "", fmt.Errorf("icloud: no download URL for %q", item.FullName())
}

// Download streams a file's contents into w and returns the byte count.
func (c *Client) Download(ctx context.Context, item Item, w io.Writer) (int64, error) {
	rawURL, err := c.DownloadURL(ctx, item)
	if err != nil {
		return 0, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, &APIError{Status: resp.StatusCode, URL: rawURL, Body: string(body)}
	}
	return io.Copy(w, resp.Body)
}

// CreateFolder creates a single folder under parent and returns it.
func (c *Client) CreateFolder(ctx context.Context, parentDrivewsID, name string) (Item, error) {
	root, err := c.driveRoot()
	if err != nil {
		return Item{}, err
	}
	payload := map[string]any{
		"destinationDrivewsId": parentDrivewsID,
		"folders": []map[string]any{
			{"clientId": newUUID(), "name": name},
		},
	}
	var resp struct {
		Folders []struct {
			Item
			Status string `json:"status"`
		} `json:"folders"`
	}
	if err := c.postJSON(ctx, root+"/createFolders", nil, payload, &resp); err != nil {
		return Item{}, err
	}
	if len(resp.Folders) == 0 {
		return Item{}, fmt.Errorf("icloud: createFolders returned nothing for %q", name)
	}
	f := resp.Folders[0]
	if f.Status != "" && f.Status != "OK" {
		return Item{}, fmt.Errorf("icloud: createFolders %q: %s", name, f.Status)
	}
	return f.Item, nil
}

// MoveToTrash moves items to the iCloud trash. Deleting requires the current
// etag, so pass freshly listed items.
func (c *Client) MoveToTrash(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	root, err := c.driveRoot()
	if err != nil {
		return err
	}
	list := make([]map[string]any, 0, len(items))
	for _, it := range items {
		list = append(list, map[string]any{
			"drivewsid": it.DrivewsID,
			"etag":      it.Etag,
			"clientId":  newUUID(),
		})
	}
	payload := map[string]any{"items": list}
	var resp struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := c.postJSON(ctx, root+"/moveItemsToTrash", nil, payload, &resp); err != nil {
		return err
	}
	for i, r := range resp.Items {
		if r.Status != "" && r.Status != "OK" {
			return fmt.Errorf("icloud: trashing %q: %s", items[i].FullName(), r.Status)
		}
	}
	return nil
}

// Rename renames an item in place.
func (c *Client) Rename(ctx context.Context, item Item, newName string) error {
	root, err := c.driveRoot()
	if err != nil {
		return err
	}
	base, ext := splitName(newName, item.IsDir())
	entry := map[string]any{
		"drivewsid": item.DrivewsID,
		"etag":      item.Etag,
		"name":      base,
	}
	if ext != "" {
		entry["extension"] = ext
	}
	payload := map[string]any{"items": []map[string]any{entry}}

	var response json.RawMessage
	if err := c.postJSON(ctx, root+"/renameItems", nil, payload, &response); err != nil {
		return err
	}
	return validateRenameResponse(newName, response)
}

// validateRenameResponse checks the per-item result returned by renameItems.
func validateRenameResponse(name string, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("icloud: renaming %q: missing operation response", name)
	}

	var response struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("icloud: renaming %q: invalid operation response: %w", name, err)
	}
	if len(response.Items) != 1 {
		return fmt.Errorf(
			"icloud: renaming %q: expected one operation result, got %d",
			name, len(response.Items),
		)
	}
	if response.Items[0].Status != "OK" {
		return fmt.Errorf(
			"icloud: renaming %q: unexpected operation status %q",
			name, response.Items[0].Status,
		)
	}
	return nil
}

// uploadTicket is the first step of an upload: Apple hands out a document id
// and a one-shot content URL.
type uploadTicket struct {
	DocumentID string `json:"document_id"`
	URL        string `json:"url"`
}

type uploadAPIStatus struct {
	StatusCode   *int   `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}

type uploadAPIResult struct {
	Status   uploadAPIStatus `json:"status"`
	Document struct {
		Name      string `json:"name"`
		Extension string `json:"extension"`
	} `json:"document"`
}

// validateUploadResponse checks both the overall API status and the result
// for the uploaded document. A successful HTTP request alone is insufficient.
func validateUploadResponse(
	expectedName string,
	statusRaw, resultsRaw json.RawMessage,
) error {
	var overall uploadAPIStatus
	if len(statusRaw) == 0 || string(statusRaw) == "null" {
		return fmt.Errorf("icloud: upload %q: missing overall API status", expectedName)
	}
	if err := json.Unmarshal(statusRaw, &overall); err != nil {
		return fmt.Errorf("icloud: upload %q: invalid overall API status: %w", expectedName, err)
	}
	if overall.StatusCode == nil {
		return fmt.Errorf("icloud: upload %q: overall API status has no status_code", expectedName)
	}
	if *overall.StatusCode != 0 {
		return fmt.Errorf(
			"icloud: upload %q: overall API status %d: %s",
			expectedName, *overall.StatusCode, overall.ErrorMessage,
		)
	}

	var results []uploadAPIResult
	if len(resultsRaw) == 0 || string(resultsRaw) == "null" {
		return fmt.Errorf("icloud: upload %q: missing operation results", expectedName)
	}
	if err := json.Unmarshal(resultsRaw, &results); err != nil {
		return fmt.Errorf("icloud: upload %q: invalid operation results: %w", expectedName, err)
	}
	if len(results) != 1 {
		return fmt.Errorf(
			"icloud: upload %q: expected one operation result, got %d",
			expectedName, len(results),
		)
	}

	result := results[0]
	if result.Status.StatusCode == nil {
		return fmt.Errorf("icloud: upload %q: operation result has no status_code", expectedName)
	}
	if *result.Status.StatusCode != 0 {
		return fmt.Errorf(
			"icloud: upload %q: operation failed with status %d: %s",
			expectedName, *result.Status.StatusCode, result.Status.ErrorMessage,
		)
	}

	actualName := result.Document.Name
	if actualName == "" {
		return fmt.Errorf("icloud: upload %q: response contains no document name", expectedName)
	}
	if result.Document.Extension != "" {
		ext := strings.TrimPrefix(result.Document.Extension, ".")
		if !strings.HasSuffix(actualName, "."+ext) {
			actualName += "." + ext
		}
	}
	if actualName != expectedName {
		return fmt.Errorf(
			"icloud: upload %q returned remote name %q; expected %q; "+
				"iCloud may already have created a file under the unexpected name",
			expectedName, actualName, expectedName,
		)
	}
	return nil
}

// Upload sends a local file into the folder identified by folderDocwsID
// (the folder's docwsid, not its drivewsid) using the three-step web flow.
func (c *Client) Upload(ctx context.Context, zone, folderDocwsID, name string, size int64, mtime time.Time, r io.Reader) error {
	if zone == "" {
		zone = DefaultZone
	}
	docs, err := c.docsRoot()
	if err != nil {
		return err
	}

	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Step 1 — request an upload slot.
	var tickets []uploadTicket
	initPayload := map[string]any{
		"filename":     name,
		"type":         "FILE",
		"content_type": contentType,
		"size":         size,
	}
	params := url.Values{"errorBreakdown": {"true"}}
	if err := c.postJSON(ctx, docs+"/ws/"+zone+"/upload/web", params, initPayload, &tickets); err != nil {
		return fmt.Errorf("upload/web %q: %w", name, err)
	}
	if len(tickets) == 0 || tickets[0].URL == "" {
		return fmt.Errorf("icloud: no upload slot granted for %q", name)
	}
	ticket := tickets[0]

	// Step 2 — POST the bytes as multipart form data.
	single, err := c.uploadContents(ctx, ticket.URL, name, contentType, r)
	if err != nil {
		return fmt.Errorf("uploading %q: %w", name, err)
	}

	// Step 3 — attach the uploaded contents to the Drive hierarchy.
	ms := mtime.UnixMilli()
	updatePayload := map[string]any{
		"allow_conflict":    true,
		"command":           "add_file",
		"create_short_guid": true,
		"document_id":       ticket.DocumentID,
		"file_flags": map[string]any{
			"is_executable": false,
			"is_hidden":     false,
			"is_writable":   true,
		},
		"mtime": ms,
		"btime": ms,
		"path": map[string]any{
			"starting_document_id": folderDocwsID,
			"path":                 name,
		},
		"data": map[string]any{
			"receipt":             single.Receipt,
			"reference_signature": single.ReferenceChecksum,
			"signature":           single.FileChecksum,
			"wrapping_key":        single.WrappingKey,
			"size":                single.Size,
		},
	}
	var updateResp struct {
		Status  json.RawMessage `json:"status"`
		Results json.RawMessage `json:"results"`
	}
	if err := c.postJSON(ctx, docs+"/ws/"+zone+"/update/documents", params, updatePayload, &updateResp); err != nil {
		return fmt.Errorf("update/documents %q: %w", name, err)
	}
	results := string(updateResp.Results)
	if results == "" {
		results = "null"
	}
	c.logf("iCloud upload response for %q: status=%s results=%s",
		name, string(updateResp.Status), results)

	if err := validateUploadResponse(name, updateResp.Status, updateResp.Results); err != nil {
		return err
	}
	return nil
}

// singleFile is the receipt Apple returns for uploaded contents.
type singleFile struct {
	ReferenceChecksum string `json:"referenceChecksum"`
	FileChecksum      string `json:"fileChecksum"`
	WrappingKey       string `json:"wrappingKey"`
	Receipt           string `json:"receipt"`
	Size              int64  `json:"size"`
}

func (c *Client) uploadContents(ctx context.Context, rawURL, name, contentType string, r io.Reader) (singleFile, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(name, name)
	if err != nil {
		return singleFile{}, err
	}
	if _, err := io.Copy(part, r); err != nil {
		return singleFile{}, err
	}
	if err := mw.Close(); err != nil {
		return singleFile{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return singleFile{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://www.icloud.com")
	req.Header.Set("Referer", "https://www.icloud.com/")

	raw, _, err := c.do(req)
	if err != nil {
		return singleFile{}, err
	}
	var resp struct {
		SingleFile singleFile `json:"singleFile"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return singleFile{}, fmt.Errorf("decoding upload receipt: %w", err)
	}
	if resp.SingleFile.Receipt == "" && resp.SingleFile.FileChecksum == "" {
		return singleFile{}, errors.New("icloud: upload receipt is empty")
	}
	return resp.SingleFile, nil
}

// splitName splits a local file name into the API's name/extension pair.
func splitName(name string, isDir bool) (base, ext string) {
	if isDir {
		return name, ""
	}
	e := path.Ext(name)
	if e == "" || e == name {
		return name, ""
	}
	return strings.TrimSuffix(name, e), strings.TrimPrefix(e, ".")
}

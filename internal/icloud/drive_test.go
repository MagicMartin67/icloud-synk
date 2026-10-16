package icloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateUploadResponse(t *testing.T) {
	tests := []struct {
		name        string
		expected    string
		status      string
		results     string
		wantErrText string
	}{
		{
			name:     "successful upload",
			expected: "bericht.txt",
			status:   `{"status_code":0,"error_message":"OK"}`,
			results:  `[{"status":{"status_code":0,"error_message":"OK"},"document":{"name":"bericht.txt","document_id":"doc-1"}}]`,
		},
		{
			name:        "unexpected suffixed filename",
			expected:    "bericht.txt",
			status:      `{"status_code":0,"error_message":"OK"}`,
			results:     `[{"status":{"status_code":0,"error_message":"OK"},"document":{"name":"bericht 3.txt","document_id":"doc-2"}}]`,
			wantErrText: `returned remote name "bericht 3.txt"`,
		},
		{
			name:        "operation failed",
			expected:    "bericht.txt",
			status:      `{"status_code":0,"error_message":"OK"}`,
			results:     `[{"status":{"status_code":1,"error_message":"failed"},"document":{"name":"bericht.txt"}}]`,
			wantErrText: "operation failed with status 1",
		},
		{
			name:        "overall API failed",
			expected:    "bericht.txt",
			status:      `{"status_code":9,"error_message":"failed"}`,
			results:     `[{"status":{"status_code":0,"error_message":"OK"},"document":{"name":"bericht.txt"}}]`,
			wantErrText: "overall API status 9",
		},
		{
			name:        "missing results",
			expected:    "bericht.txt",
			status:      `{"status_code":0,"error_message":"OK"}`,
			results:     `null`,
			wantErrText: "missing operation results",
		},
		{
			name:        "missing document name",
			expected:    "bericht.txt",
			status:      `{"status_code":0,"error_message":"OK"}`,
			results:     `[{"status":{"status_code":0,"error_message":"OK"},"document":{"document_id":"doc-3"}}]`,
			wantErrText: "response contains no document name",
		},
		{
			name:     "base name plus extension",
			expected: "bericht.txt",
			status:   `{"status_code":0,"error_message":"OK"}`,
			results:  `[{"status":{"status_code":0,"error_message":"OK"},"document":{"name":"bericht","extension":"txt"}}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUploadResponse(
				tt.expected,
				json.RawMessage(tt.status),
				json.RawMessage(tt.results),
			)
			if tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("unerwarteter Fehler: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("erwarteter Fehler fehlt")
			}
			if !strings.Contains(err.Error(), tt.wantErrText) {
				t.Fatalf("Fehler %q enthält nicht %q", err, tt.wantErrText)
			}
		})
	}
}

func TestValidateRenameResponse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "success", raw: `{"items":[{"status":"OK"}]}`},
		{name: "failed status", raw: `{"items":[{"status":"FAILED"}]}`, wantErr: true},
		{name: "missing status", raw: `{"items":[{}]}`, wantErr: true},
		{name: "missing items", raw: `{"items":[]}`, wantErr: true},
		{name: "missing response", raw: `null`, wantErr: true},
		{name: "invalid JSON", raw: `{invalid`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRenameResponse("bericht.txt", json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRenameResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRenameSendsExpectedRequest(t *testing.T) {
	var requestBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Methode = %q, erwartet POST", r.Method)
		}
		if r.URL.Path != "/renameItems" {
			t.Errorf("Pfad = %q, erwartet /renameItems", r.URL.Path)
		}

		var err error
		requestBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("Anfrage konnte nicht gelesen werden: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"status":"OK"}]}`))
	}))
	defer server.Close()

	c := &Client{
		http: server.Client(),
		sess: &Session{
			DSID:     "test-dsid",
			ClientID: "auth-test-client",
			Webservices: map[string]string{
				"drivews": server.URL,
			},
		},
	}

	item := Item{
		DrivewsID: "FILE::test-file",
		Etag:      "test-etag",
		Type:      TypeFile,
		Name:      "alt",
		Extension: "txt",
	}

	if err := c.Rename(context.Background(), item, "neu.txt"); err != nil {
		t.Fatalf("Rename() meldet unerwarteten Fehler: %v", err)
	}

	var payload struct {
		Items []struct {
			DrivewsID string `json:"drivewsid"`
			Etag      string `json:"etag"`
			Name      string `json:"name"`
			Extension string `json:"extension"`
		} `json:"items"`
	}

	if err := json.Unmarshal(requestBody, &payload); err != nil {
		t.Fatalf("Anfrage enthält ungültiges JSON: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("Anzahl der Anfrageelemente = %d, erwartet 1", len(payload.Items))
	}

	got := payload.Items[0]
	if got.DrivewsID != item.DrivewsID || got.Etag != item.Etag ||
		got.Name != "neu" || got.Extension != "txt" {
		t.Fatalf("unerwartete Umbenennungsanfrage: %+v", got)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rob121/cannon/extension"
)

func TestCapabilities(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()

	newServer().Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"page":          "/page",
		"data":          "/data",
		"admin":         "/admin",
		"help":          "/help",
		"configuration": "/configuration",
		"templates":     "/templates",
	} {
		if got.Capabilities[key] != want {
			t.Fatalf("capability %s = %q, want %q", key, got.Capabilities[key], want)
		}
	}
}

func TestConfigurationUsesCategoryDropdownField(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)

	req := httptest.NewRequest(http.MethodGet, "/configuration", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var doc extension.ConfigurationDocument
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 1 {
		t.Fatalf("sections = %#v", doc.Sections)
	}
	schema := string(doc.Sections[0].Schema)
	if !strings.Contains(schema, `"category_id"`) || !strings.Contains(schema, `"format":"category"`) {
		t.Fatalf("schema missing category format: %s", schema)
	}
	uiSchema := string(doc.Sections[0].UISchema)
	if !strings.Contains(uiSchema, `"scope":"#/properties/category_id"`) || !strings.Contains(uiSchema, `"format":"category"`) {
		t.Fatalf("ui schema missing category format option: %s", uiSchema)
	}
	if !strings.Contains(schema, `"allow_files"`) || !strings.Contains(schema, `"max_file_size_mb"`) {
		t.Fatalf("schema missing file upload settings: %s", schema)
	}
}

func TestPageMigratesSettingsTableWhenInstallWasSkipped(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()

	resp := postWire(t, server, "/page/submission-form", extension.WireRequest{
		Method:   http.MethodGet,
		URL:      "/submit-news",
		CSRF:     "page-token",
		SiteID:   "example",
		PageItem: "submission-form",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wire status = %d body = %s", resp.StatusCode, resp.Body)
	}
	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable(settingsTableName()) {
		t.Fatalf("expected settings table %s to be created", settingsTableName())
	}
}

func TestPageRendersSubmissionForm(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	resp := postWire(t, server, "/page/submission-form", extension.WireRequest{
		Method:   http.MethodGet,
		URL:      "/submit-news",
		CSRF:     "page-token",
		SiteID:   "example",
		PageItem: "submission-form",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wire status = %d body = %s", resp.StatusCode, resp.Body)
	}
	actionURL := extension.PublicDataURL(extensionName, "example", "submission/submit")
	for _, want := range []string{
		"Submit a News Tip",
		`action="` + actionURL + `"`,
		`name="_csrf" value="page-token"`,
		`name="title"`,
		`name="body"`,
		`name="contributor_name"`,
		`name="contributor_email"`,
		`id="submission-files"`,
		`name="attachments_json"`,
		`Allowed: pdf, xlsx, csv, jpg, jpeg, png, gif, webp, svg`,
		`cdn.jsdelivr.net/npm/quill@2.0.3/dist/quill.js`,
		`submission-body-toolbar`,
		`Promise.all(files.map(readFile))`,
		`new Quill`,
		`<captcha context="form" provider="any"></captcha>`,
	} {
		if !strings.Contains(resp.Body, want) {
			t.Fatalf("body missing %q: %s", want, resp.Body)
		}
	}
	if strings.Contains(resp.Body, `name="intro"`) || strings.Contains(resp.Body, "Brief summary") {
		t.Fatalf("summary field should not render: %s", resp.Body)
	}
	if strings.Index(resp.Body, `name="contributor_name"`) < strings.Index(resp.Body, `name="title"`) ||
		strings.Index(resp.Body, `name="contributor_name"`) > strings.Index(resp.Body, `name="body"`) {
		t.Fatalf("contributor fields should render between title and body: %s", resp.Body)
	}
}

func TestSubmitCreatesQueuedSubmissionWithFiles(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	files := []submittedFilePayload{{
		Name: "road.csv",
		Type: "text/csv",
		Size: int64(len("street,status\nMain,closed\n")),
		Data: "data:text/csv;base64,c3RyZWV0LHN0YXR1cwpNYWluLGNsb3NlZAo=",
	}}
	rawFiles, _ := json.Marshal(files)
	values := url.Values{}
	values.Set("title", "Road closure downtown")
	values.Set("body", `<p>There is a <strong>closure</strong> on Main Street.</p><script>alert("nope")</script>`)
	values.Set("contributor_name", `Jane <script>Bad</script>`)
	values.Set("contributor_email", "jane@example.com")
	values.Set("attachments_json", string(rawFiles))
	values.Set("return_url", "/submit-news")
	resp := postWire(t, server, "/data/submission/submit", extension.WireRequest{
		Method:   http.MethodPost,
		URL:      extension.PublicDataURL(extensionName, "example", "submission/submit"),
		Body:     values.Encode(),
		SiteID:   "example",
		DataPath: "submission/submit",
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("wire status = %d body = %s", resp.StatusCode, resp.Body)
	}
	if loc := resp.Header["Location"]; len(loc) != 1 || loc[0] != "/submit-news?submitted=1" {
		t.Fatalf("redirect = %#v", resp.Header)
	}
	if len(resp.EmitEvents) != 1 || resp.EmitEvents[0].Event != "onSubmissionAfterCreate" {
		t.Fatalf("emit_events = %#v", resp.EmitEvents)
	}
	if title, _ := resp.EmitEvents[0].Arguments["title"].(string); title != "Road closure downtown" {
		t.Fatalf("emit title = %#v", resp.EmitEvents[0].Arguments)
	}

	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	var row submissionRow
	if err := db.Table(submissionTableName()).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "pending" || row.Title != "Road closure downtown" {
		t.Fatalf("unexpected submission: %#v", row)
	}
	if strings.Contains(row.Body, "<script") || strings.Contains(row.Body, "alert(") {
		t.Fatalf("body was not sanitized: %s", row.Body)
	}
	var savedFiles []submissionFileRow
	if err := db.Table(submissionFilesTableName()).Where("submission_id = ?", row.SubmissionID).Find(&savedFiles).Error; err != nil {
		t.Fatal(err)
	}
	if len(savedFiles) != 1 || savedFiles[0].OriginalName != "road.csv" {
		t.Fatalf("saved files = %#v", savedFiles)
	}
	if _, err := os.Stat(savedFiles[0].StoragePath); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
}

func TestAdminListsQueuedSubmissions(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Table(submissionTableName()).Create(&submissionRow{
		Title:  "Pending tip",
		Body:   "<p>Tip details</p>",
		Status: "pending",
	}).Error; err != nil {
		t.Fatal(err)
	}
	importedID := uint(123)
	if err := db.Table(submissionTableName()).Create(&submissionRow{
		Title:          "Imported tip",
		Body:           "<p>Imported details</p>",
		Status:         "imported",
		ImportedItemID: &importedID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	resp := postWire(t, server, "/admin/", extension.WireRequest{
		Method: http.MethodGet,
		URL:    "/admin/extension-apps/" + extensionName,
		SiteID: "example",
		User: map[string]any{
			"authenticated": true,
			"email":         "editor@example.com",
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wire status = %d body = %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(resp.Body, `href="/admin/extension-apps/cannon-ext-submission/submissions/1" data-turbo="false">Pending tip</a>`) {
		t.Fatalf("admin body missing expected content: %s", resp.Body)
	}
	if strings.Contains(resp.Body, "Imported tip") {
		t.Fatalf("imported submissions should not remain in queue: %s", resp.Body)
	}
}

func TestAdminDetailImportsSubmissionToChosenCategoryAndMedia(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	db, site, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	sub := submissionRow{
		Title:            "Original tip",
		Body:             "<p>Original body</p>",
		ContributorName:  "Jane",
		ContributorEmail: "jane@example.com",
		Status:           "pending",
	}
	if err := db.Table(submissionTableName()).Create(&sub).Error; err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(site.TmpDir, extensionName, "submissions", fmt.Sprint(sub.SubmissionID))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(dir, "tip.csv")
	if err := os.WriteFile(stored, []byte("headline,score\nTip,1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := db.Table(submissionFilesTableName()).Create(&submissionFileRow{
		SubmissionID: sub.SubmissionID,
		OriginalName: "tip.csv",
		StoredName:   "tip.csv",
		StoragePath:  stored,
		MIME:         "text/csv",
		Size:         21,
	}).Error; err != nil {
		t.Fatal(err)
	}

	detail := postWire(t, server, "/admin/submissions/1", extension.WireRequest{
		Method: http.MethodGet,
		URL:    "/admin/extension-apps/" + extensionName + "/submissions/1",
		SiteID: "example",
		User: map[string]any{
			"authenticated": true,
		},
	})
	for _, want := range []string{
		`cdn.jsdelivr.net/npm/quill@2.0.3/dist/quill.js`,
		`submission-body-toolbar`,
		`new Quill`,
		`data-turbo="false" class="submission-detail-form"`,
		`data:text/csv;base64`,
		`headline,score`,
		`download="tip.csv"`,
	} {
		if !strings.Contains(detail.Body, want) {
			t.Fatalf("detail body missing %q: %s", want, detail.Body)
		}
	}

	values := url.Values{}
	values.Set("action", "import")
	values.Set("title", "Edited tip")
	values.Set("body", "<p>Edited body</p>")
	values.Set("contributor_name", "Jane")
	values.Set("contributor_email", "jane@example.com")
	values.Set("category_id", fmt.Sprint(catID))
	values.Set("media_folder", "content/submissions")
	resp := postWire(t, server, "/admin/submissions/1", extension.WireRequest{
		Method: http.MethodPost,
		URL:    "/admin/extension-apps/" + extensionName + "/submissions/1",
		SiteID: "example",
		Body:   values.Encode(),
		User: map[string]any{
			"authenticated": true,
		},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("import status = %d body = %s", resp.StatusCode, resp.Body)
	}
	if got := http.Header(resp.Header).Get("Location"); got == "" || !strings.HasPrefix(got, "/admin/items/") {
		t.Fatalf("import should redirect to item, header = %#v", resp.Header)
	}
	var item itemRow
	if err := db.Table("items").Where("title = ?", "Edited tip").First(&item).Error; err != nil {
		t.Fatal(err)
	}
	if item.CategoryID == nil || *item.CategoryID != catID || item.Status != "pending" {
		t.Fatalf("unexpected item: %#v", item)
	}
	if !strings.Contains(item.Body, "Contributor Details") || !strings.Contains(item.AttachmentsJSON, "/assets/media/content/submissions/tip.csv") {
		t.Fatalf("item missing contributor/files: body=%s attachments=%s", item.Body, item.AttachmentsJSON)
	}
	var media mediaAssetRow
	if err := db.Table("media_assets").Where("path = ?", "/assets/media/content/submissions/tip.csv").First(&media).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(site.AssetsDir, "media", "content", "submissions", "tip.csv")); err != nil {
		t.Fatalf("media file missing: %v", err)
	}
}

func setupExtensionDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sites.json")
	dbPath := filepath.Join(dir, "example.sqlite")
	raw := fmt.Sprintf(`{
  "install_enabled": false,
  "data_root": %q,
  "sites": [{
    "id": "example",
    "name": "Example",
    "host": "http://localhost:8001",
    "database": {"type": "sqlite", "dsn": %q},
    "template_dir": %q,
    "assets_dir": %q,
    "tmp_dir": %q,
    "language_dir": %q
  }]
}`, dir, dbPath, filepath.Join(dir, "templates"), filepath.Join(dir, "assets"), filepath.Join(dir, "tmp"), filepath.Join(dir, "language"))
	if err := os.WriteFile(cfgPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANNON_CONFIG", cfgPath)
	oldArgs := os.Args
	os.Args = []string{"submission-extension", "--site=example", "--socket=/tmp/submission.sock"}
	t.Cleanup(func() { os.Args = oldArgs })
}

func installExtension(t *testing.T, handler http.Handler) {
	t.Helper()
	resp := postWire(t, handler, "/install", extension.WireRequest{
		Method: http.MethodPost,
		URL:    "/install",
		SiteID: "example",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("install status = %d body = %s", resp.StatusCode, resp.Body)
	}
	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Table("categories").AutoMigrate(&categoryRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("items").AutoMigrate(&itemRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("media_assets").AutoMigrate(&mediaAssetRow{}); err != nil {
		t.Fatal(err)
	}
}

func createCategory(t *testing.T, name, slug, status string) uint {
	t.Helper()
	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	row := categoryRow{Name: name, Slug: slug, Status: status}
	if err := db.Table("categories").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.CategoryID
}

func saveConfig(t *testing.T, handler http.Handler, categoryID uint) {
	t.Helper()
	payload := map[string]any{
		"section": configSection,
		"data": map[string]any{
			"title":        "Submit a News Tip",
			"introduction": "Share what you know.",
			"category_id":  categoryID,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/configuration", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save config status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func settingsTableName() string {
	return extension.TableName(extension.Info{Name: extensionName}.TablePrefix(), "settings")
}

func postWire(t *testing.T, handler http.Handler, path string, wire extension.WireRequest) extension.WireResponse {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("http status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp extension.WireResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

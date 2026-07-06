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
		`cdn.jsdelivr.net/npm/quill@2.0.3/dist/quill.js`,
		`submission-body-toolbar`,
		`ql-image`,
		`submission-image-input`,
		`insertEmbed(range.index, 'image'`,
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
}

func TestSubmitCreatesPendingContentItem(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	values := url.Values{}
	values.Set("title", "Road closure downtown")
	values.Set("body", `<p>There is a <strong>closure</strong> on Main Street.</p><p><img src="data:image/png;base64,iVBORw0KGgo=" alt="Road photo"></p><script>alert("nope")</script>`)
	values.Set("contributor_name", `Jane <script>Bad</script>`)
	values.Set("contributor_email", "jane@example.com")
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

	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	var row itemRow
	if err := db.Table("items").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "pending" || row.CategoryID == nil || *row.CategoryID != catID {
		t.Fatalf("unexpected item: %#v", row)
	}
	if row.Slug != "road-closure-downtown" {
		t.Fatalf("slug = %q", row.Slug)
	}
	if row.Intro != "" || row.MetaDescription != "" {
		t.Fatalf("summary fields should be empty: intro=%q meta=%q", row.Intro, row.MetaDescription)
	}
	for _, want := range []string{"<p>There is a <strong>closure</strong> on Main Street.</p>", `src="data:image/png;base64,iVBORw0KGgo="`, `alt="Road photo"`, "Contributor Details", "Jane &lt;script&gt;Bad&lt;/script&gt;", "jane@example.com"} {
		if !strings.Contains(row.Body, want) {
			t.Fatalf("body missing %q: %s", want, row.Body)
		}
	}
	if strings.Contains(row.Body, "<script") || strings.Contains(row.Body, "alert(") {
		t.Fatalf("body was not sanitized: %s", row.Body)
	}
}

func TestAdminListsPendingConfiguredCategoryItems(t *testing.T) {
	setupExtensionDB(t)
	server := newServer().Handler()
	installExtension(t, server)
	catID := createCategory(t, "Tips", "tips", "active")
	saveConfig(t, server, catID)

	db, _, err := extension.OpenDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Table("items").Create(&itemRow{
		Title:      "Pending tip",
		Slug:       "pending-tip",
		Status:     "pending",
		CategoryID: &catID,
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
	if !strings.Contains(resp.Body, "Pending tip") || !strings.Contains(resp.Body, "Tips") {
		t.Fatalf("admin body missing expected content: %s", resp.Body)
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

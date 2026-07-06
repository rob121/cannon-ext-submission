package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/rob121/cannon/extension"
	"gorm.io/gorm"
)

//go:embed help/*.md
var helpFiles embed.FS

//go:embed all:templates
var templateFiles embed.FS

const (
	extensionName        = "cannon-ext-submission"
	extensionTitle       = "News Submission"
	extensionDescription = "Adds a public news tip submission form that creates Cannon content items in the review queue for a configured category."
	extensionVersion     = "0.1.0"
	updateURLBase        = "https://github.com/rob121/cannon-ext-submission/releases/download"
	defaultAdminMenu     = "News Submissions"
	configSection        = "general"
)

var (
	submissionConfigUI = json.RawMessage(`{
  "type": "VerticalLayout",
  "elements": [
    {"type": "Control", "scope": "#/properties/title"},
    {"type": "Control", "scope": "#/properties/introduction"},
    {"type": "Control", "scope": "#/properties/category_id", "options": {"format": "category"}}
  ]
}`)
	slugRE = regexp.MustCompile(`[^a-z0-9]+`)
)

type submissionApp struct {
	info          extension.Info
	templates     *extension.Templates
	settingsTable string
}

type submissionSettings struct {
	Title        string `json:"title"`
	Introduction string `json:"introduction"`
	CategoryID   uint   `json:"category_id"`
}

type categoryRow struct {
	CategoryID uint   `gorm:"column:category_id;primaryKey"`
	Name       string `gorm:"column:name"`
	Slug       string `gorm:"column:slug"`
	Status     string `gorm:"column:status"`
}

type itemRow struct {
	ItemID          uint      `gorm:"column:item_id;primaryKey"`
	Title           string    `gorm:"column:title"`
	Slug            string    `gorm:"column:slug"`
	Intro           string    `gorm:"column:intro"`
	Body            string    `gorm:"column:body"`
	Status          string    `gorm:"column:status"`
	CategoryID      *uint     `gorm:"column:category_id"`
	MetaDescription string    `gorm:"column:meta_description"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

type configStore struct {
	table string
}

type configProvider struct {
	store configStore
}

func main() {
	if err := newServer().Run(); err != nil {
		log.Fatal(err)
	}
}

func newServer() *extension.Server {
	info := extension.Info{
		Name:          extensionName,
		Title:         extensionTitle,
		Description:   extensionDescription,
		Version:       extensionVersion,
		UpdateURLBase: updateURLBase,
		AdminMenuName: defaultAdminMenu,
	}
	app := &submissionApp{
		info:          info,
		templates:     extension.NewTemplates(templateFiles, "templates"),
		settingsTable: extension.TableName(info.TablePrefix(), "settings"),
	}

	s := extension.New(info)
	s.EmbedHelp(helpFiles, "help")
	s.EmbedTemplates(templateFiles, "templates")
	s.OnInstall(app.install)
	s.OnConfiguration(configProvider{store: configStore{table: app.settingsTable}})
	s.RegisterPage(extension.PageDefinition{
		ID:    "submission-form",
		Title: "News Submission Form",
	}, app.page)
	s.HandleData("submission/submit", app.submit)
	s.HandleAdmin("/admin/", app.admin)
	return s
}

func (app *submissionApp) install(req extension.WireRequest) error {
	db, _, err := extension.OpenDB()
	if err != nil {
		return err
	}
	return extension.MigrateConfigurationStore(db, app.settingsTable)
}

func (app *submissionApp) page(item string, req extension.WireRequest) extension.WireResponse {
	settings, err := app.loadSettings()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	category, configured, err := app.configuredCategory(settings)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	data := map[string]any{
		"Title":        settings.pageTitle(),
		"Introduction": settings.pageIntroduction(),
		"ActionURL":    extension.PublicDataURL(extensionName, req.SiteID, "submission/submit"),
		"ReturnURL":    req.URL,
		"CSRFField":    template.HTML(extension.CSRFHiddenField(req)),
		"Configured":   configured,
		"Category":     category,
		"Submitted":    queryValue(req.URL, "submitted") == "1",
		"PageItem":     item,
	}
	return app.render("submission/page.html", data)
}

func (app *submissionApp) submit(req extension.WireRequest) extension.WireResponse {
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return extension.Error(http.StatusMethodNotAllowed, "POST required")
	}
	settings, err := app.loadSettings()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	category, configured, err := app.configuredCategory(settings)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	if !configured {
		return extension.Error(http.StatusBadRequest, "submission category is not configured")
	}

	values, err := url.ParseQuery(req.Body)
	if err != nil {
		return extension.Error(http.StatusBadRequest, "invalid submission")
	}
	title := strings.TrimSpace(values.Get("title"))
	body := sanitizeSubmissionHTML(values.Get("body"))
	name := strings.TrimSpace(values.Get("contributor_name"))
	email := strings.TrimSpace(values.Get("contributor_email"))
	if title == "" {
		return extension.Error(http.StatusBadRequest, "title is required")
	}
	if body == "" {
		return extension.Error(http.StatusBadRequest, "submission details are required")
	}

	db, _, err := extension.OpenDB()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	slug, err := uniqueItemSlug(db, title, 0)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	categoryID := category.CategoryID
	row := itemRow{
		Title:      truncate(title, 512),
		Slug:       slug,
		Body:       appendContributorDetails(body, name, email),
		Status:     "pending",
		CategoryID: &categoryID,
	}
	if err := db.Table("items").Create(&row).Error; err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}

	redirectURL := strings.TrimSpace(values.Get("return_url"))
	if redirectURL == "" {
		redirectURL = req.URL
	}
	return extension.Redirect(http.StatusSeeOther, submittedRedirect(redirectURL))
}

func (app *submissionApp) admin(req extension.WireRequest) extension.WireResponse {
	if !extension.UserAuthenticated(req) {
		return extension.Error(http.StatusForbidden, "sign in required")
	}
	settings, err := app.loadSettings()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	category, configured, err := app.configuredCategory(settings)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	rows, total, err := recentPending(category.CategoryID, configured)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	return app.render("submission/admin.html", map[string]any{
		"Title":       "News Submissions",
		"Subtitle":    "Public submissions are saved as pending content items.",
		"Configured":  configured,
		"Category":    category,
		"Rows":        rows,
		"Total":       total,
		"ReviewURL":   "/admin/review",
		"SettingsURL": "/admin/configuration/extensions/" + url.PathEscape(extensionName),
	})
}

func (app *submissionApp) configuredCategory(settings submissionSettings) (categoryRow, bool, error) {
	if settings.CategoryID == 0 {
		return categoryRow{}, false, nil
	}
	db, _, err := extension.OpenDB()
	if err != nil {
		return categoryRow{}, false, err
	}
	var row categoryRow
	err = db.Table("categories").
		Where("category_id = ? AND status = ?", settings.CategoryID, "active").
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return categoryRow{}, false, nil
	}
	return row, err == nil, err
}

func (app *submissionApp) loadSettings() (submissionSettings, error) {
	data, err := configStore{table: app.settingsTable}.Load(configSection)
	if err != nil {
		return submissionSettings{}, err
	}
	return settingsFromMap(data), nil
}

func (app *submissionApp) render(name string, data map[string]any) extension.WireResponse {
	body, err := app.templates.WithFuncs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Format("Jan 2, 2006 3:04 PM")
		},
	}).Execute(name, data)
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	return extension.HTML(http.StatusOK, body)
}

func (p configProvider) Configuration() (extension.ConfigurationDocument, error) {
	data, err := p.store.Load(configSection)
	if err != nil {
		return extension.ConfigurationDocument{}, err
	}
	rawData, _ := json.Marshal(data)
	return extension.ConfigurationDocument{Sections: []extension.ConfigurationSection{
		{
			ID:       configSection,
			Title:    "Submission Settings",
			Schema:   submissionConfigSchema(),
			UISchema: submissionConfigUI,
			Data:     rawData,
		},
	}}, nil
}

func (p configProvider) SaveConfiguration(req extension.ConfigurationSaveRequest) error {
	if req.Section != configSection {
		return fmt.Errorf("unknown configuration section %q", req.Section)
	}
	var data map[string]any
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			return fmt.Errorf("invalid configuration data: %w", err)
		}
	}
	settings := settingsFromMap(data)
	if settings.CategoryID == 0 {
		return fmt.Errorf("category is required")
	}
	if _, ok, err := categoryByID(settings.CategoryID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("selected category is not active")
	}
	data["category_id"] = float64(settings.CategoryID)
	return p.store.Save(req.Section, data)
}

func submissionConfigSchema() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"title":       "Form Title",
				"default":     "Submit a News Tip",
				"description": "Heading shown above the public submission form.",
			},
			"introduction": map[string]any{
				"type":        "string",
				"title":       "Introduction",
				"description": "Short instructions shown above the form.",
			},
			"category_id": map[string]any{
				"type":        "integer",
				"format":      "category",
				"title":       "Submission Category",
				"description": "Submitted items are created in this category with Pending Review status.",
			},
		},
		"required": []string{"category_id"},
	}
	raw, _ := json.Marshal(schema)
	return raw
}

func categoryByID(id uint) (categoryRow, bool, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return categoryRow{}, false, err
	}
	var row categoryRow
	err = db.Table("categories").
		Where("category_id = ? AND status = ?", id, "active").
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return categoryRow{}, false, nil
	}
	return row, err == nil, err
}

func recentPending(categoryID uint, configured bool) ([]itemRow, int64, error) {
	if !configured {
		return nil, 0, nil
	}
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, 0, err
	}
	q := db.Table("items").Where("status = ? AND category_id = ?", "pending", categoryID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []itemRow
	err = db.Table("items").
		Where("status = ? AND category_id = ?", "pending", categoryID).
		Order("created_at desc, item_id desc").
		Limit(10).
		Find(&rows).Error
	return rows, total, err
}

func (s configStore) Load(section string) (map[string]any, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, err
	}
	if err := extension.MigrateConfigurationStore(db, s.table); err != nil {
		return nil, err
	}
	return extension.DBConfigurationStore(db, s.table).Load(section)
}

func (s configStore) Save(section string, data map[string]any) error {
	db, _, err := extension.OpenDB()
	if err != nil {
		return err
	}
	if err := extension.MigrateConfigurationStore(db, s.table); err != nil {
		return err
	}
	return extension.DBConfigurationStore(db, s.table).Save(section, data)
}

func settingsFromMap(data map[string]any) submissionSettings {
	settings := submissionSettings{
		Title:        "Submit a News Tip",
		Introduction: "Share a local story, event, correction, or lead for editors to review.",
	}
	if data == nil {
		return settings
	}
	if v, ok := data["title"].(string); ok && strings.TrimSpace(v) != "" {
		settings.Title = strings.TrimSpace(v)
	}
	if v, ok := data["introduction"].(string); ok {
		settings.Introduction = strings.TrimSpace(v)
	}
	settings.CategoryID = uintFromAny(data["category_id"])
	return settings
}

func (s submissionSettings) pageTitle() string {
	if strings.TrimSpace(s.Title) == "" {
		return "Submit a News Tip"
	}
	return strings.TrimSpace(s.Title)
}

func (s submissionSettings) pageIntroduction() string {
	if strings.TrimSpace(s.Introduction) == "" {
		return "Share a local story, event, correction, or lead for editors to review."
	}
	return strings.TrimSpace(s.Introduction)
}

func uniqueItemSlug(db *gorm.DB, base string, excludeID uint) (string, error) {
	slug := slugify(base)
	for i := 0; i < 100; i++ {
		candidate := slug
		if i > 0 {
			candidate = slug + "-" + strconv.Itoa(i)
		}
		var count int64
		q := db.Table("items").Where("slug = ?", candidate)
		if excludeID > 0 {
			q = q.Where("item_id <> ?", excludeID)
		}
		if err := q.Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
	}
	return slug + "-" + strconv.FormatInt(time.Now().Unix(), 10), nil
}

func slugify(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = slugRE.ReplaceAllString(v, "-")
	v = strings.Trim(v, "-")
	if v == "" {
		return "submission"
	}
	return v
}

func sanitizeSubmissionHTML(raw string) string {
	policy := bluemonday.NewPolicy()
	policy.AllowElements("p", "br", "strong", "b", "em", "i", "u", "s", "blockquote", "ol", "ul", "li", "h2", "h3", "a")
	policy.AllowAttrs("href").OnElements("a")
	policy.AllowAttrs("target", "rel").Matching(bluemonday.SpaceSeparatedTokens).OnElements("a")
	policy.AllowAttrs("alt").OnElements("img")
	policy.AllowImages()
	policy.AllowDataURIImages()
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	html := strings.TrimSpace(policy.Sanitize(raw))
	if html == "<p><br></p>" || html == "<p><br/></p>" {
		return ""
	}
	return html
}

func appendContributorDetails(body, name, email string) string {
	name = template.HTMLEscapeString(strings.TrimSpace(name))
	email = template.HTMLEscapeString(strings.TrimSpace(email))
	if name == "" && email == "" {
		return body
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(body))
	b.WriteString(`<blockquote><h3>Contributor Details</h3><ul>`)
	if name != "" {
		b.WriteString(`<li><strong>Name:</strong> `)
		b.WriteString(name)
		b.WriteString(`</li>`)
	}
	if email != "" {
		b.WriteString(`<li><strong>Email:</strong> `)
		b.WriteString(email)
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></blockquote>`)
	return b.String()
}

func submittedRedirect(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || strings.TrimSpace(u.Path) == "" {
		return "/?submitted=1"
	}
	q := u.Query()
	q.Set("submitted", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

func queryValue(rawURL, key string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}

func uintFromAny(v any) uint {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return uint(n)
		}
	case int:
		if n > 0 {
			return uint(n)
		}
	case uint:
		return n
	case json.Number:
		if parsed, err := strconv.ParseUint(string(n), 10, 64); err == nil {
			return uint(parsed)
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64); err == nil {
			return uint(parsed)
		}
	}
	return 0
}

func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max]
}

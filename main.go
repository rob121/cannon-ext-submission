package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	extensionDescription = "Adds a public news tip submission form that queues Cannon content items for editorial review."
	extensionVersion     = "0.1.0"
	updateURLBase        = "https://github.com/rob121/cannon-ext-submission/releases/download"
	defaultAdminMenu     = "News Submissions"
	configSection        = "general"
	defaultAllowedFiles  = "pdf, xlsx, csv, jpg, jpeg, png, gif, webp, svg"
	defaultMaxFileSizeMB = 10
	defaultMediaFolder   = "content/submissions"
	maxInlinePreviewSize = 32 << 20
	maxTextPreviewSize   = 64 << 10
)

var (
	submissionConfigUI = json.RawMessage(`{
  "type": "VerticalLayout",
  "elements": [
    {"type": "Control", "scope": "#/properties/title"},
    {"type": "Control", "scope": "#/properties/introduction"},
    {"type": "Control", "scope": "#/properties/category_id", "options": {"format": "category"}},
    {"type": "Control", "scope": "#/properties/allow_files"},
    {"type": "Control", "scope": "#/properties/allowed_extensions"},
    {"type": "Control", "scope": "#/properties/max_file_size_mb"}
  ]
}`)
	slugRE        = regexp.MustCompile(`[^a-z0-9]+`)
	fileSegmentRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
)

type submissionApp struct {
	info          extension.Info
	templates     *extension.Templates
	settingsTable string
}

type submissionSettings struct {
	Title             string `json:"title"`
	Introduction      string `json:"introduction"`
	CategoryID        uint   `json:"category_id"`
	AllowFiles        bool   `json:"allow_files"`
	AllowedExtensions string `json:"allowed_extensions"`
	MaxFileSizeMB     int    `json:"max_file_size_mb"`
}

type categoryRow struct {
	CategoryID uint   `gorm:"column:category_id;primaryKey"`
	Name       string `gorm:"column:name"`
	Slug       string `gorm:"column:slug"`
	Status     string `gorm:"column:status"`
}

type itemRow struct {
	ItemID          uint      `gorm:"column:item_id;primaryKey"`
	Locale          string    `gorm:"column:locale"`
	Title           string    `gorm:"column:title"`
	Slug            string    `gorm:"column:slug"`
	Intro           string    `gorm:"column:intro"`
	Body            string    `gorm:"column:body"`
	Status          string    `gorm:"column:status"`
	CategoryID      *uint     `gorm:"column:category_id"`
	Image           string    `gorm:"column:image"`
	GalleryJSON     string    `gorm:"column:gallery_json"`
	AttachmentsJSON string    `gorm:"column:attachments_json"`
	MetaDescription string    `gorm:"column:meta_description"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

type mediaAssetRow struct {
	MediaID   uint      `gorm:"column:media_id;primaryKey"`
	Name      string    `gorm:"column:name"`
	Path      string    `gorm:"column:path"`
	MIME      string    `gorm:"column:mime"`
	Size      int64     `gorm:"column:size"`
	Folder    string    `gorm:"column:folder"`
	Alt       string    `gorm:"column:alt"`
	Width     int       `gorm:"column:width"`
	Height    int       `gorm:"column:height"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

type submissionRow struct {
	SubmissionID     uint       `gorm:"column:submission_id;primaryKey"`
	Title            string     `gorm:"column:title;size:512;not null"`
	Body             string     `gorm:"column:body;type:text;not null"`
	ContributorName  string     `gorm:"column:contributor_name;size:256"`
	ContributorEmail string     `gorm:"column:contributor_email;size:256"`
	Status           string     `gorm:"column:status;size:32;not null;default:pending;index"`
	ImportedItemID   *uint      `gorm:"column:imported_item_id;index"`
	ReviewedAt       *time.Time `gorm:"column:reviewed_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

type submissionFileRow struct {
	FileID       uint      `gorm:"column:file_id;primaryKey"`
	SubmissionID uint      `gorm:"column:submission_id;index;not null"`
	OriginalName string    `gorm:"column:original_name;size:512;not null"`
	StoredName   string    `gorm:"column:stored_name;size:512;not null"`
	StoragePath  string    `gorm:"column:storage_path;size:1024;not null"`
	MIME         string    `gorm:"column:mime;size:128"`
	Size         int64     `gorm:"column:size;not null;default:0"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

type submissionFileView struct {
	submissionFileRow
	DataURL     template.URL
	PreviewText string
	SizeLabel   string
	IsImage     bool
	IsPDF       bool
	IsText      bool
	CanOpen     bool
	TooLarge    bool
}

type submittedFilePayload struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Data string `json:"data"`
}

type itemGalleryImage struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

type itemAttachment struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
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
	if err := extension.MigrateConfigurationStore(db, app.settingsTable); err != nil {
		return err
	}
	return migrateSubmissionStore(db)
}

func (app *submissionApp) page(item string, req extension.WireRequest) extension.WireResponse {
	settings, err := app.loadSettings()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	data := map[string]any{
		"Title":             settings.pageTitle(),
		"Introduction":      settings.pageIntroduction(),
		"ActionURL":         extension.PublicDataURL(extensionName, req.SiteID, "submission/submit"),
		"ReturnURL":         req.URL,
		"CSRFField":         template.HTML(extension.CSRFHiddenField(req)),
		"Configured":        true,
		"Submitted":         queryValue(req.URL, "submitted") == "1",
		"PageItem":          item,
		"AllowFiles":        settings.AllowFiles,
		"AllowedExtensions": settings.allowedExtensionsLabel(),
		"AcceptAttribute":   settings.acceptAttribute(),
		"MaxFileSizeMB":     settings.maxFileSizeMB(),
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
	files, err := decodeSubmittedFiles(values.Get("attachments_json"), settings)
	if err != nil {
		return extension.Error(http.StatusBadRequest, err.Error())
	}

	db, _, err := extension.OpenDB()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	if err := migrateSubmissionStore(db); err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	row := submissionRow{
		Title:            truncate(title, 512),
		Body:             body,
		ContributorName:  truncate(name, 256),
		ContributorEmail: truncate(email, 256),
		Status:           "pending",
	}
	if err := db.Table(submissionTableName()).Create(&row).Error; err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	if err := app.saveSubmissionFiles(row.SubmissionID, files); err != nil {
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
	path := adminSubPath(req.URL)
	if strings.HasPrefix(path, "/submissions/") {
		return app.adminSubmission(req, strings.TrimPrefix(path, "/submissions/"))
	}
	db, _, err := extension.OpenDB()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	if err := migrateSubmissionStore(db); err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	rows, total, err := recentPendingSubmissions()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	return app.render("submission/admin.html", map[string]any{
		"Title":       "News Submissions",
		"Subtitle":    "Public submissions are queued for editorial review.",
		"Configured":  true,
		"Rows":        rows,
		"Total":       total,
		"BaseURL":     adminBaseURL(),
		"SettingsURL": "/admin/configuration/extensions/" + url.PathEscape(extensionName),
	})
}

func (app *submissionApp) adminSubmission(req extension.WireRequest, idPath string) extension.WireResponse {
	idText := strings.Trim(strings.Split(strings.Trim(idPath, "/"), "/")[0], " ")
	id64, err := strconv.ParseUint(idText, 10, 64)
	if err != nil || id64 == 0 {
		return extension.Error(http.StatusNotFound, "submission not found")
	}
	id := uint(id64)
	values := url.Values{}
	if strings.EqualFold(req.Method, http.MethodPost) {
		values, err = url.ParseQuery(req.Body)
		if err != nil {
			return extension.Error(http.StatusBadRequest, "invalid submission action")
		}
		action := strings.TrimSpace(values.Get("action"))
		switch action {
		case "reject":
			if err := rejectSubmission(id); err != nil {
				return app.renderSubmissionDetail(req, id, "", err.Error())
			}
			return app.renderSubmissionDetail(req, id, "Submission rejected.", "")
		case "import":
			itemID, err := importSubmission(id, values)
			if err != nil {
				return app.renderSubmissionDetail(req, id, "", err.Error())
			}
			return redirectToAdminItem(itemID)
		default:
			return app.renderSubmissionDetail(req, id, "", "Unknown action.")
		}
	}
	return app.renderSubmissionDetail(req, id, "", "")
}

func (app *submissionApp) renderSubmissionDetail(req extension.WireRequest, id uint, notice, errMsg string) extension.WireResponse {
	row, files, err := submissionByID(id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return extension.Error(http.StatusNotFound, "submission not found")
		}
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	cats, err := activeCategories()
	if err != nil {
		return extension.Error(http.StatusInternalServerError, err.Error())
	}
	settings, _ := app.loadSettings()
	categoryID := settings.CategoryID
	if categoryID == 0 && len(cats) > 0 {
		categoryID = cats[0].CategoryID
	}
	fileViews := submissionFileViews(files)
	return app.render("submission/detail.html", map[string]any{
		"Title":       "Review Submission",
		"BaseURL":     adminBaseURL(),
		"CSRFField":   template.HTML(extension.CSRFHiddenField(req)),
		"Submission":  row,
		"Files":       fileViews,
		"Categories":  cats,
		"CategoryID":  categoryID,
		"MediaFolder": defaultMediaFolder,
		"Notice":      notice,
		"Error":       errMsg,
		"EditItemURL": func() string {
			if row.ImportedItemID == nil {
				return ""
			}
			return "/admin/items/" + strconv.FormatUint(uint64(*row.ImportedItemID), 10)
		}(),
	})
}

func redirectToAdminItem(itemID uint) extension.WireResponse {
	itemURL := "/admin/items/" + strconv.FormatUint(uint64(itemID), 10)
	return extension.Redirect(http.StatusSeeOther, itemURL)
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
		"safeHTML": func(v string) template.HTML {
			return template.HTML(v)
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
	if data == nil {
		data = map[string]any{}
	}
	settings := settingsFromMap(data)
	if settings.CategoryID > 0 {
		if _, ok, err := categoryByID(settings.CategoryID); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("selected category is not active")
		}
		data["category_id"] = float64(settings.CategoryID)
	}
	data["allow_files"] = settings.AllowFiles
	data["allowed_extensions"] = settings.allowedExtensionsLabel()
	data["max_file_size_mb"] = settings.maxFileSizeMB()
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
				"title":       "Default Import Category",
				"description": "Optional default category selected when importing a queued submission.",
			},
			"allow_files": map[string]any{
				"type":        "boolean",
				"title":       "Allow File Attachments",
				"default":     true,
				"description": "When enabled, public submitters can attach approved files.",
			},
			"allowed_extensions": map[string]any{
				"type":        "string",
				"title":       "Allowed File Extensions",
				"default":     defaultAllowedFiles,
				"description": "Comma-separated extensions accepted from the public form.",
			},
			"max_file_size_mb": map[string]any{
				"type":        "integer",
				"title":       "Maximum File Size (MB)",
				"default":     defaultMaxFileSizeMB,
				"minimum":     1,
				"maximum":     512,
				"description": "Maximum size for each attached file.",
			},
		},
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

func recentPendingSubmissions() ([]submissionRow, int64, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, 0, err
	}
	q := db.Table(submissionTableName()).Where("status = ?", "pending")
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []submissionRow
	err = db.Table(submissionTableName()).
		Where("status = ?", "pending").
		Order("created_at desc, submission_id desc").
		Limit(10).
		Find(&rows).Error
	return rows, total, err
}

func countSubmissions(status string) (int64, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return 0, err
	}
	q := db.Table(submissionTableName())
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	return total, q.Count(&total).Error
}

func submissionByID(id uint) (submissionRow, []submissionFileRow, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return submissionRow{}, nil, err
	}
	if err := migrateSubmissionStore(db); err != nil {
		return submissionRow{}, nil, err
	}
	var row submissionRow
	if err := db.Table(submissionTableName()).Where("submission_id = ?", id).First(&row).Error; err != nil {
		return submissionRow{}, nil, err
	}
	var files []submissionFileRow
	if err := db.Table(submissionFilesTableName()).Where("submission_id = ?", id).Order("file_id asc").Find(&files).Error; err != nil {
		return submissionRow{}, nil, err
	}
	return row, files, nil
}

func activeCategories() ([]categoryRow, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, err
	}
	var rows []categoryRow
	err = db.Table("categories").Where("status = ?", "active").Order("name asc").Find(&rows).Error
	return rows, err
}

func rejectSubmission(id uint) error {
	db, _, err := extension.OpenDB()
	if err != nil {
		return err
	}
	var sub submissionRow
	if err := db.Table(submissionTableName()).Where("submission_id = ?", id).First(&sub).Error; err != nil {
		return err
	}
	if sub.Status != "pending" {
		return fmt.Errorf("only pending submissions can be rejected")
	}
	now := time.Now()
	return db.Table(submissionTableName()).Where("submission_id = ?", id).Updates(map[string]any{
		"status":      "rejected",
		"reviewed_at": now,
		"updated_at":  now,
	}).Error
}

func importSubmission(id uint, values url.Values) (uint, error) {
	db, site, err := extension.OpenDB()
	if err != nil {
		return 0, err
	}
	if err := migrateSubmissionStore(db); err != nil {
		return 0, err
	}
	var sub submissionRow
	if err := db.Table(submissionTableName()).Where("submission_id = ?", id).First(&sub).Error; err != nil {
		return 0, err
	}
	if sub.Status == "imported" && sub.ImportedItemID != nil {
		return *sub.ImportedItemID, nil
	}
	if sub.Status == "rejected" {
		return 0, fmt.Errorf("rejected submissions cannot be imported")
	}
	categoryID := uintFromAny(values.Get("category_id"))
	if categoryID == 0 {
		return 0, fmt.Errorf("choose a category before importing")
	}
	if _, ok, err := categoryByID(categoryID); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("selected category is not active")
	}
	title := strings.TrimSpace(values.Get("title"))
	if title == "" {
		return 0, fmt.Errorf("title is required")
	}
	body := sanitizeSubmissionHTML(values.Get("body"))
	if body == "" {
		return 0, fmt.Errorf("submission details are required")
	}
	name := strings.TrimSpace(values.Get("contributor_name"))
	email := strings.TrimSpace(values.Get("contributor_email"))
	folder := normalizeSubmissionMediaFolder(values.Get("media_folder"))
	files, err := submissionFiles(id)
	if err != nil {
		return 0, err
	}
	moved, err := moveSubmissionFilesToMedia(site.AssetsDir, folder, files)
	if err != nil {
		return 0, err
	}
	slug, err := uniqueItemSlug(db, title, 0)
	if err != nil {
		return 0, err
	}
	itemBody := appendContributorDetails(body, name, email)
	categoryIDCopy := categoryID
	item := itemRow{
		Locale:          "en-US",
		Title:           truncate(title, 512),
		Slug:            slug,
		Body:            itemBody,
		Status:          "pending",
		CategoryID:      &categoryIDCopy,
		Image:           firstImagePath(moved),
		GalleryJSON:     encodeJSON(galleryFromMovedFiles(moved)),
		AttachmentsJSON: encodeJSON(attachmentsFromMovedFiles(moved)),
	}
	if err := db.Table("items").Create(&item).Error; err != nil {
		return 0, err
	}
	for _, file := range moved {
		if err := db.Table("media_assets").Create(&mediaAssetRow{
			Name:   file.Name,
			Path:   file.WebPath,
			MIME:   file.MIME,
			Size:   file.Size,
			Folder: folder,
			Alt:    strings.TrimSuffix(file.Name, filepath.Ext(file.Name)),
			Width:  file.Width,
			Height: file.Height,
		}).Error; err != nil {
			return 0, err
		}
	}
	now := time.Now()
	if err := db.Table(submissionTableName()).Where("submission_id = ?", id).Updates(map[string]any{
		"status":            "imported",
		"imported_item_id":  item.ItemID,
		"reviewed_at":       now,
		"updated_at":        now,
		"title":             truncate(title, 512),
		"body":              body,
		"contributor_name":  truncate(name, 256),
		"contributor_email": truncate(email, 256),
	}).Error; err != nil {
		return 0, err
	}
	return item.ItemID, nil
}

func submissionFiles(id uint) ([]submissionFileRow, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, err
	}
	var files []submissionFileRow
	err = db.Table(submissionFilesTableName()).Where("submission_id = ?", id).Order("file_id asc").Find(&files).Error
	return files, err
}

func submissionFileViews(files []submissionFileRow) []submissionFileView {
	out := make([]submissionFileView, 0, len(files))
	for _, file := range files {
		view := submissionFileView{
			submissionFileRow: file,
			SizeLabel:         formatBytes(file.Size),
		}
		mimeType := strings.TrimSpace(file.MIME)
		if mimeType == "" {
			mimeType = detectMIMEFile(file.StoragePath)
			view.MIME = mimeType
		}
		lowerName := strings.ToLower(file.OriginalName)
		view.IsImage = strings.HasPrefix(mimeType, "image/")
		view.IsPDF = mimeType == "application/pdf" || strings.HasSuffix(lowerName, ".pdf")
		view.IsText = strings.HasPrefix(mimeType, "text/") || strings.HasSuffix(lowerName, ".csv")
		if file.Size > maxInlinePreviewSize {
			view.TooLarge = true
			out = append(out, view)
			continue
		}
		raw, err := os.ReadFile(file.StoragePath)
		if err != nil {
			out = append(out, view)
			continue
		}
		if mimeType == "" {
			mimeType = detectMIMEBytes(raw, file.OriginalName)
			view.MIME = mimeType
		}
		view.DataURL = template.URL("data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(raw))
		view.CanOpen = true
		if view.IsText {
			preview := raw
			if len(preview) > maxTextPreviewSize {
				preview = preview[:maxTextPreviewSize]
			}
			view.PreviewText = string(preview)
		}
		out = append(out, view)
	}
	return out
}

func formatBytes(size int64) string {
	if size < 0 {
		size = 0
	}
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func (s configStore) Load(section string) (map[string]any, error) {
	db, _, err := extension.OpenDB()
	if err != nil {
		return nil, err
	}
	if err := extension.MigrateConfigurationStore(db, s.table); err != nil {
		return nil, err
	}
	if err := migrateSubmissionStore(db); err != nil {
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
		Title:             "Submit a News Tip",
		Introduction:      "Share a local story, event, correction, or lead for editors to review.",
		AllowFiles:        true,
		AllowedExtensions: defaultAllowedFiles,
		MaxFileSizeMB:     defaultMaxFileSizeMB,
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
	if v, ok := data["allow_files"]; ok {
		settings.AllowFiles = boolFromAny(v, true)
	}
	if v, ok := data["allowed_extensions"].(string); ok && strings.TrimSpace(v) != "" {
		settings.AllowedExtensions = strings.TrimSpace(v)
	}
	settings.MaxFileSizeMB = intFromAny(data["max_file_size_mb"], defaultMaxFileSizeMB)
	if settings.MaxFileSizeMB < 1 {
		settings.MaxFileSizeMB = 1
	}
	if settings.MaxFileSizeMB > 512 {
		settings.MaxFileSizeMB = 512
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

func (s submissionSettings) maxFileSizeMB() int {
	if s.MaxFileSizeMB < 1 {
		return defaultMaxFileSizeMB
	}
	if s.MaxFileSizeMB > 512 {
		return 512
	}
	return s.MaxFileSizeMB
}

func (s submissionSettings) allowedExtensions() []string {
	raw := strings.TrimSpace(s.AllowedExtensions)
	if raw == "" {
		raw = defaultAllowedFiles
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), "."))
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	if len(out) == 0 {
		return settingsWithDefaultFiles().allowedExtensions()
	}
	return out
}

func settingsWithDefaultFiles() submissionSettings {
	return submissionSettings{AllowedExtensions: defaultAllowedFiles, MaxFileSizeMB: defaultMaxFileSizeMB, AllowFiles: true}
}

func (s submissionSettings) allowedExtensionsLabel() string {
	return strings.Join(s.allowedExtensions(), ", ")
}

func (s submissionSettings) acceptAttribute() string {
	exts := s.allowedExtensions()
	out := make([]string, 0, len(exts)+4)
	for _, ext := range exts {
		if ext == "jpg" || ext == "jpeg" || ext == "png" || ext == "gif" || ext == "webp" || ext == "svg" {
			continue
		}
		out = append(out, "."+ext)
	}
	if allowsAnyImage(exts) {
		out = append(out, "image/*")
	}
	return strings.Join(out, ",")
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

func migrateSubmissionStore(db *gorm.DB) error {
	if err := db.Table(submissionTableName()).AutoMigrate(&submissionRow{}); err != nil {
		return err
	}
	return db.Table(submissionFilesTableName()).AutoMigrate(&submissionFileRow{})
}

func submissionTableName() string {
	return extension.TableName(extension.Info{Name: extensionName}.TablePrefix(), "submissions")
}

func submissionFilesTableName() string {
	return extension.TableName(extension.Info{Name: extensionName}.TablePrefix(), "submission_files")
}

func (app *submissionApp) saveSubmissionFiles(submissionID uint, files []submittedFilePayload) error {
	if len(files) == 0 {
		return nil
	}
	db, site, err := extension.OpenDB()
	if err != nil {
		return err
	}
	dir := filepath.Join(site.TmpDir, extensionName, "submissions", strconv.FormatUint(uint64(submissionID), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, file := range files {
		raw, err := decodeFileData(file.Data)
		if err != nil {
			return fmt.Errorf("%s: %w", file.Name, err)
		}
		name := safeFileName(file.Name)
		storedName := uniqueStoredFileName(dir, fmt.Sprintf("%02d-%s", i+1, name))
		path := filepath.Join(dir, storedName)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return err
		}
		mimeType := strings.TrimSpace(file.Type)
		if mimeType == "" {
			mimeType = detectMIMEBytes(raw, name)
		}
		if err := db.Table(submissionFilesTableName()).Create(&submissionFileRow{
			SubmissionID: submissionID,
			OriginalName: name,
			StoredName:   storedName,
			StoragePath:  path,
			MIME:         mimeType,
			Size:         int64(len(raw)),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func decodeSubmittedFiles(raw string, settings submissionSettings) ([]submittedFilePayload, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !settings.AllowFiles {
		return nil, fmt.Errorf("file attachments are not enabled")
	}
	var files []submittedFilePayload
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, fmt.Errorf("invalid file attachment payload")
	}
	maxSize := int64(settings.maxFileSizeMB()) * 1024 * 1024
	allowed := settings.allowedExtensions()
	for i, file := range files {
		file.Name = safeFileName(file.Name)
		if file.Name == "" {
			return nil, fmt.Errorf("attached file %d is missing a name", i+1)
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(file.Name), "."))
		if !extensionAllowed(ext, allowed) {
			return nil, fmt.Errorf("file type %q is not allowed; approved extensions: %s", ext, strings.Join(allowed, ", "))
		}
		if file.Size > maxSize {
			return nil, fmt.Errorf("%s exceeds the maximum upload size of %d MB", file.Name, settings.maxFileSizeMB())
		}
		rawBytes, err := decodeFileData(file.Data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.Name, err)
		}
		if int64(len(rawBytes)) > maxSize {
			return nil, fmt.Errorf("%s exceeds the maximum upload size of %d MB", file.Name, settings.maxFileSizeMB())
		}
		file.Size = int64(len(rawBytes))
		files[i] = file
	}
	return files, nil
}

func decodeFileData(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, fmt.Errorf("file data is empty")
	}
	if idx := strings.Index(data, ","); strings.HasPrefix(data, "data:") && idx >= 0 {
		data = data[idx+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("file data is not valid base64")
	}
	return raw, nil
}

type movedSubmissionFile struct {
	Name    string
	WebPath string
	MIME    string
	Size    int64
	Width   int
	Height  int
}

func moveSubmissionFilesToMedia(assetsDir, folder string, files []submissionFileRow) ([]movedSubmissionFile, error) {
	if len(files) == 0 {
		return nil, nil
	}
	destDir := filepath.Join(assetsDir, "media", filepath.FromSlash(folder))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	moved := make([]movedSubmissionFile, 0, len(files))
	for _, file := range files {
		name := safeFileName(file.OriginalName)
		destName := uniqueStoredFileName(destDir, name)
		destPath := filepath.Join(destDir, destName)
		if err := moveOrCopyFile(file.StoragePath, destPath); err != nil {
			return nil, err
		}
		mimeType := strings.TrimSpace(file.MIME)
		if mimeType == "" {
			mimeType = detectMIMEFile(destPath)
		}
		width, height := imageDimensions(destPath)
		moved = append(moved, movedSubmissionFile{
			Name:    destName,
			WebPath: "/assets/media/" + folder + "/" + destName,
			MIME:    mimeType,
			Size:    file.Size,
			Width:   width,
			Height:  height,
		})
	}
	return moved, nil
}

func moveOrCopyFile(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

func galleryFromMovedFiles(files []movedSubmissionFile) []itemGalleryImage {
	out := make([]itemGalleryImage, 0)
	for _, file := range files {
		if strings.HasPrefix(file.MIME, "image/") {
			out = append(out, itemGalleryImage{URL: file.WebPath, Alt: strings.TrimSuffix(file.Name, filepath.Ext(file.Name))})
		}
	}
	return out
}

func attachmentsFromMovedFiles(files []movedSubmissionFile) []itemAttachment {
	out := make([]itemAttachment, 0)
	for _, file := range files {
		if strings.HasPrefix(file.MIME, "image/") {
			continue
		}
		out = append(out, itemAttachment{URL: file.WebPath, Label: file.Name})
	}
	return out
}

func firstImagePath(files []movedSubmissionFile) string {
	for _, file := range files {
		if strings.HasPrefix(file.MIME, "image/") {
			return file.WebPath
		}
	}
	return ""
}

func encodeJSON(v any) string {
	raw, _ := json.Marshal(v)
	if string(raw) == "null" {
		return "[]"
	}
	return string(raw)
}

func detectMIMEBytes(raw []byte, filename string) string {
	if mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename))); mimeType != "" {
		return mimeType
	}
	if len(raw) > 512 {
		raw = raw[:512]
	}
	return http.DetectContentType(raw)
}

func detectMIMEFile(path string) string {
	if mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); mimeType != "" {
		return mimeType
	}
	file, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	buf := make([]byte, 512)
	n, _ := file.Read(buf)
	return http.DetectContentType(buf[:n])
}

func imageDimensions(path string) (int, int) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func safeFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	name = fileSegmentRE.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		return "submission-file"
	}
	return name
}

func uniqueStoredFileName(dir, name string) string {
	name = safeFileName(name)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", base, time.Now().Unix(), ext)
}

func normalizeSubmissionMediaFolder(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	if raw == "" {
		return defaultMediaFolder
	}
	parts := strings.Split(raw, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(fileSegmentRE.ReplaceAllString(strings.TrimSpace(part), "-"))
		part = strings.Trim(part, ".-")
		if part != "" {
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return defaultMediaFolder
	}
	return strings.Join(clean, "/")
}

func extensionAllowed(ext string, allowed []string) bool {
	for _, item := range allowed {
		if strings.EqualFold(ext, item) {
			return true
		}
	}
	return false
}

func allowsAnyImage(exts []string) bool {
	for _, ext := range exts {
		switch ext {
		case "jpg", "jpeg", "png", "gif", "webp", "svg":
			return true
		}
	}
	return false
}

func adminBaseURL() string {
	return "/admin/extension-apps/" + url.PathEscape(extensionName)
}

func adminSubPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	base := adminBaseURL()
	path := strings.TrimRight(u.Path, "/")
	if path == strings.TrimRight(base, "/") {
		return ""
	}
	if strings.HasPrefix(path, base+"/") {
		return strings.TrimPrefix(path, base)
	}
	return ""
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

func intFromAny(v any, def int) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	case uint:
		if n > 0 {
			return int(n)
		}
	case json.Number:
		if parsed, err := strconv.Atoi(string(n)); err == nil && parsed > 0 {
			return parsed
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return def
}

func boolFromAny(v any, def bool) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	case float64:
		return b != 0
	case int:
		return b != 0
	}
	return def
}

func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max]
}

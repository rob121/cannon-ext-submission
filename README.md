# Cannon News Submission Extension

Adds a public news tip submission form to Cannon. Submitted tips are saved in an extension-owned review queue, then editors can import accepted submissions into a chosen Cannon category.

## Features

- Public submission form for title, contributor details, and formatted body content.
- Lightweight rich text editor support for the body field.
- Optional file attachments when enabled, including PDFs, spreadsheets, CSV files, and images.
- Extension-owned submission and file storage before editorial review.
- Admin dashboard listing queued submissions with extension-local detail pages.
- Import flow for editing a submission, choosing a category, and moving files into Cannon media.
- Reject flow for submissions that should not become content.
- Optional default category configuration using Cannon's category selector field.
- Optional captcha rendering through Cannon's captcha capability.
- Overridable built-in templates.

## Cannon Capabilities

This extension exposes:

- `/meta`: extension name, description, version, and update URL base.
- `/capabilities`: page, admin, configuration, help, templates, and data capabilities.
- `/admin`: submission dashboard and review detail UI.
- `/configuration`: form, default import category, and attachment settings.
- `/page`: public news submission page definition and rendering.
- `/data/submission/submit`: public submission endpoint.
- `/templates`: built-in template metadata for overrides.
- `/help`: extension help content.
- `/install`: creates the extension settings and submission queue tables.

## Configuration

Use Cannon's extension configuration screen to adjust the public form text, optionally set a default import category, and control attachment uploads. Attachments can be enabled or disabled, allowed extensions are configurable, and each file is checked against the configured MB limit.

## Usage

1. Install and activate `cannon-ext-submission` in Cannon.
2. Configure the form and attachment settings.
3. Add a Cannon route using the News Submission Form page type.
4. Review submissions at `/admin/extension-apps/cannon-ext-submission`.
5. Open a queued submission, edit it as needed, choose a category, then import or reject it.

## Releases

The extension reports:

- Name: `cannon-ext-submission`
- Version: `0.1.0`
- Update URL base: `https://github.com/rob121/cannon-ext-submission/releases/download`

GitHub releases should include a `cannon-extension.json` manifest plus platform binaries.

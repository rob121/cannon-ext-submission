# Cannon News Submission Extension

Adds a public news tip submission form to Cannon. Submitted tips are saved as pending Cannon content items in a configured category so editors can review them before publishing.

## Features

- Public submission form for title, contributor details, and formatted body content.
- Lightweight rich text editor support for the body field.
- Optional embedded images through the editor.
- Creates Cannon content items with pending review status.
- Adds contributor name and email into the saved article body.
- Admin dashboard showing destination category and recent pending submissions.
- Category configuration using Cannon's category selector field.
- Optional captcha rendering through Cannon's captcha capability.
- Overridable built-in templates.

## Cannon Capabilities

This extension exposes:

- `/meta`: extension name, description, version, and update URL base.
- `/capabilities`: page, admin, configuration, help, templates, and data capabilities.
- `/admin`: submission dashboard UI.
- `/configuration`: destination category settings.
- `/page`: public news submission page definition and rendering.
- `/data/submission/submit`: public submission endpoint.
- `/templates`: built-in template metadata for overrides.
- `/help`: extension help content.
- `/install`: creates the extension settings table.

## Configuration

Use Cannon's extension configuration screen to choose the category where public submissions should be created. The field uses Cannon's custom JSON Forms category selector.

## Usage

1. Install and activate `cannon-ext-submission` in Cannon.
2. Configure the destination category.
3. Add a Cannon route using the News Submission Form page type.
4. Review submitted content in Cannon's normal review queue.

## Releases

The extension reports:

- Name: `cannon-ext-submission`
- Version: `0.1.0`
- Update URL base: `https://github.com/rob121/cannon-ext-submission/releases/download`

GitHub releases should include a `cannon-extension.json` manifest plus platform binaries.

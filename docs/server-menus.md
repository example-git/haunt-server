# Server-provided menus

Servers advertising `menus.declarative.v1` serve authenticated, permission-filtered
`GET /api/v1/menus/server` and `GET /api/v1/channels/{id}/menu` manifests. Clients
fetch them when settings/context menus open. Menus are not a permission boundary:
every action endpoint independently authorizes the current principal.

Version 1 contains `pages`, each with an ID, label, optional context-menu label,
and exactly one of:

- `builtin`: an existing complex client editor (appearance, channel icons, roles,
  storage providers, etc.). A new builtin still requires client support.
- `form`: load/save API paths, method, declarative fields, optional scalar carry
  fields such as a revision, descriptions and save/success labels. A new form,
  page, field or API path works in existing clients supporting these controls.
- `action`: API path, method and required confirmation text; destructive actions
  may additionally require typing a server-provided confirmation value.

Supported form controls: text, textarea, password, number, toggle and select.
Fields use dotted JSON paths. Numeric display scaling supports MiB-to-bytes
conversion without changing API units. Only declared fields and carry fields are
submitted; arbitrary properties from GET responses are never echoed back.
Changing runtime configuration carries its revision to prevent lost edits.

`internal/httpapi/menus.go` is the server-side definition registry. Add a page
there and an independently authenticated endpoint to expand settings without
changing the client. The generated OpenAPI documentation describes the manifest.
The live Limits & access form and channel deletion already use this protocol.
Complex builtin editors and older servers retain their existing behavior.

The renderer never evaluates server strings, HTML or expressions. Paths must be
literal same-server `/api/v1/` resources, with no URL escapes, traversal, query
strings or redirects. Requests use the existing browser/Electron transport,
omit cookies and carry only that server's token. JSON responses are capped at
256 KiB; menus, fields and choices have count limits. Prototype-related field
paths and unsupported schema versions/controls are rejected. New control kinds
require a future protocol version and client support.

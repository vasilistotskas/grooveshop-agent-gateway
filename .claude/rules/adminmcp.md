---
paths:
  - "internal/adminmcp/**"
  - "internal/django/client_staff.go"
---

# Store-admin MCP server (`/mcp/admin`)

Loads when working on the staff server. It must not import
`internal/mcpsrv`: what the two servers share lives in
`internal/mcptool` and `internal/identity`. The code wins over this page.

- `internal/adminmcp` — `/mcp/admin`, a separate MCP server with its own
  tools and middleware. It is not behind `tenant.RequireAgentCommerce`:
  its gate is the store's `staffAiAccess` feature
  (`StoreFeatures.StaffAIAccess`), off by default because what the
  app reads goes to its provider. Without it `RequireStaff` and the
  resource metadata answer 404 (`TestAStoreWithAccessOffServesNothing`),
  and a store without customer accounts still serves its staff. `RequireStaff` refuses a call without a staff
  bearer (401 + RFC 9728 challenge naming every read scope and
  `/.well-known/oauth-protected-resource/mcp/admin`), verifies one against
  Django `/agent/staff/me` (`identity.Verifier`, refused through
  `identity.Refuse`) and
  forwards it to `/api/v1/agent/staff/*`, which checks the scope, the role
  and the team on every call. A scope not granted or not open to the role
  is refused before Django is asked; a 403 from Django is reported as
  `not_allowed` here (the person needs to know it is their role), unlike
  `/mcp`, and a 404 is checked against `/agent/staff/me` first, so a store
  that switched access off answers `access_off`, never "no data".
  Customer-written text comes back as `{untrusted: true, source, text}`
  (`mcptool.Untrusted`). Staff tokens come from Django's staff
  issuer, never the store's own (that one signs in customers), and are
  bound to the store's `/mcp/admin` on its primary domain. Every tool is
  read only; a write tool needs the owner's approval first
  (`grooveshop-docs/src/dev/flows/staff-mcp.md`).

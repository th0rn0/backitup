
## Screenshots

Screenshots live in `docs/screenshots/` and are embedded in `README.md`. They must be kept up to date whenever the UI changes.

**When to retake screenshots:** any change to HTML templates (`internal/server/templates/*.html`) or CSS (`internal/server/assets/app.css`) requires updated screenshots before committing.

**How to retake:**
1. `BACKITUP_DB=/tmp/backitup-ss.db BACKITUP_ADMIN_USER=admin BACKITUP_ADMIN_PASSWORD=screenshotpass BACKITUP_ADDR=:18080 go run ./cmd/server &`
2. Use the browser (playwright) to navigate each page and save screenshots to `docs/screenshots/`:
   - `login.png` — `/login`
   - `dashboard-empty.png` — `/` before any clients are added
   - `dashboard.png` — `/` with 2–3 clients added
   - `add-client.png` — `/clients/new`
   - `client-created.png` — the page shown immediately after creating a client
   - `client-detail.png` — `/clients/1`
3. `kill $(lsof -t -i:18080) && rm -f /tmp/backitup-ss.db`
4. Commit the updated screenshots alongside the template/CSS changes.

## Skill routing

When the user's request matches an available skill, invoke it via the Skill tool. When in doubt, invoke the skill.

Key routing rules:
- Product ideas/brainstorming → invoke /office-hours
- Strategy/scope → invoke /plan-ceo-review
- Architecture → invoke /plan-eng-review
- Design system/plan review → invoke /design-consultation or /plan-design-review
- Full review pipeline → invoke /autoplan
- Bugs/errors → invoke /investigate
- QA/testing site behavior → invoke /qa or /qa-only
- Code review/diff check → invoke /review
- Visual polish → invoke /design-review
- Ship/deploy/PR → invoke /ship or /land-and-deploy
- Save progress → invoke /context-save
- Resume context → invoke /context-restore
- Author a backlog-ready spec/issue → invoke /spec

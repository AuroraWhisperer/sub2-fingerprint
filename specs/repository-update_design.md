# Repository update button

## Goal and acceptance criteria

Synchronize this project to the existing public `AuroraWhisperer/sub2-fingerprint` repository and expose an administrator-only update flow above the existing detection form. A check reads the latest stable GitHub release; installation uses the existing checksum-verified updater and reconnects the page after restart. An active detection prevents installation, and a pending update prevents new detections.

## Existing implementation

`deploy/update.py` already downloads only trusted release assets, checks hashes and architecture, installs atomically, checks health, and rolls back on failure. The web process runs as an unprivileged system user with a read-only installation directory. It must not gain general sudo or shell-command access.

## Design

- `updates.go`: authenticated local status, stable-release check, and update-request endpoints. Reuse existing administrator and CSRF validation. A shared mutex serializes detection submission against requesting an update. The repository and filesystem paths are fixed by the application, not accepted from a browser.
- `web/update.js`: update buttons in a compact inline section using the current design. Show current/latest version and checking/installing/error/current states; the header version links to releases. Only install after an explicit button click. Explain that restart clears in-memory results. Refresh the session after a rollback restart before enabling retries.
- `deploy/units/sub2-fingerprint-update.path`: watch one fixed empty request file in the detector's systemd runtime directory.
- `deploy/units/sub2-fingerprint-update.service`: execute the existing root-owned updater with a fixed `--web` option. A separate root-owned runtime directory stores a small sanitized status file through restart. The request contains no command, URL, credentials, or arguments.
- `deploy/deploy.py` and `deploy/update.py`: install, verify, upgrade, roll back, and uninstall the two update units together with existing managed resources. Old format-2 manifests without update units remain supported.
- Increment the version above the published `v0.2.2`, synchronize the reviewed source, and publish a tagged release so the existing updater has installable assets.

## Security and failure behavior

- Require a verified Sub2API administrator and same-origin CSRF protection for checks and installation.
- Disable installation during detection; check this on the server under the same mutex used by submissions.
- Fixed GitHub repository and stable semantic versions only; keep existing release checksum validation and rollback.
- Never expose raw command output, local configuration, or tokens in status responses.
- Pending requests expire if the systemd watcher does not respond. The service records failure on abnormal exit. Failed updates unblock new detection after cleanup.
- Preserve installation configuration and unrelated workspace files. Exclude build outputs and local settings from the public commit.

## Validation

- Go endpoint tests: authentication, CSRF, malformed input, unavailable installer, latest version, active detection conflict, update/detection exclusion, and repeated install requests.
- Python tests: installer/update/rollback/uninstall unit ownership and previous-state preservation; web status success/failure and fixed request handling; same-version repair after old updaters omit new unit assets, including failed-repair rollback.
- JavaScript tests: session renewal after rollback before retrying installation; completing detection clears the installation blocker.
- Browser checks: exact local route responds, header and update section render, check/install/error states work, and layout remains usable at desktop/mobile widths.
- Run Go, JavaScript, and deployment tests, build releases, push, and verify GitHub Actions and published assets.

## Deployment boundary

Synchronizing GitHub and publishing a release do not update an existing server automatically. With the v0.2.x updater, first upgrade the program, then repeat the command to install missing web update units from the same release. Future updates can use the button; fresh installations include both units.

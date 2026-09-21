# Changelog

## v1.0.0 (2026-09-21)

Initial release.

- LinuxServer.io Docker Mod (s6-overlay v3 `init-mod-plex-4k-guard` oneshot
  and `svc-mod-plex-4k-guard` longrun) shipping a static Go binary for
  linux/amd64 and linux/arm64 in a single-layer `FROM scratch` image.
- Polls `/status/sessions` on the local Plex server, resolves each video
  transcode's source through `/library/metadata/{ratingKey}` and terminates
  only sessions whose source is 4K/UHD via `/status/sessions/terminate`.
- Token read from `Preferences.xml`, refreshed on change or HTTP 401, sent
  only as a header, redacted from every log line.
- Stop message from `/config/4k-stop-message.txt`, created atomically with
  the default text when missing; blank or unreadable files fall back to the
  default without being modified.
- `PLEX_4K_GUARD_DRY_RUN=true` dry-run mode, optional runtime mode file,
  per-session cooldown, bounded timeouts and exponential backoff, graceful
  shutdown on SIGTERM/SIGINT.

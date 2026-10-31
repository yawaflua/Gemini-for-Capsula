# Marusya Plugins frontend

Svelte 5 frontend of the Marusya plugin registry (https://marusya.ywfl.dev): publishing, review and download of plugins for the Marusya speaker. It consumes the HTTP gateway defined in `../backend/api/project/v1/project.proto` and includes Russian and English localization.

## Development with Bun

```bash
bun install
bun run dev
```

The development server proxies `/v1` to `http://localhost:8080`. Override that target with `VITE_BACKEND_PROXY`; for a production API on another origin, set `VITE_API_BASE_URL`. See `.env.example`.

## Checks

```bash
bun run check
bun run lint
bun test
bun run build
```

## Implemented flows

- registration, login, refresh-token recovery, logout, and profile editing;
- public plugin search, project details, version listing, and downloads (no account needed);
- owner workspace: draft creation, slug availability, editing, archive upload, and review submission;
- project-scoped moderation discussion backed by notifications;
- moderator list-detail queue, owner messages, approve/reject decisions, and decision notifications;
- adaptive drawer/rail/bottom navigation, light/dark schemes, RU/EN switching, keyboard tabs, and recoverable loading/error/empty states.

A version archive is a ZIP with the plugin's `plugin.json` at its root or in its only top-level folder; the `id` in `plugin.json` must equal the project URL (slug). The backend derives where the plugin runs (server, speaker or both) from `plugin.json` and stores the archive's SHA-512, which downloads repeat in the `X-Checksum-Sha512` header.

Publication terms and moderation rules are in [EULA.md](EULA.md).

# Plan: Add Grok Build hook install target

**Issue**: #146
**Date**: 2026-08-29
**Status**: implementing

## Context

Grok Build (xAI) has a documented hook contract (`~/.grok/hooks/*.json`) but PromptConduit has no install target. Piggybacking on Claude/Cursor files would mis-attribute sessions.

## Approach

- Slug `grok` (alias `grok-build`)
- Dedicated `~/.grok/hooks/promptconduit.json` (Copilot pattern)
- `promptconduit hook --tool grok`
- Detect via `--tool`, `GROK_*` env, and `hookEventName`
- PascalCase event keys; timeout in seconds
- Do not disable Grok's Claude/Cursor compat scanners

## Tasks

- [ ] Register `grok` in SupportedTools / IsValidTool
- [ ] install / uninstall / status / init
- [ ] detectTool + getHookEventName + ExtractIDs
- [ ] Sample config + README / help
- [ ] Tests

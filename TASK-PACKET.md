# Task Packet: FAC-109

**Title**: Stall/spin detection + auto re-nudge for fleet agents (herd-shoot/herd-spin)
**Priority**: urgent
**Status**: to-do
**Labels**: 

## Worktree

**Path**: `.herd/worktrees/fac-109`
**Branch**: `task/fac-109-stall-spin-detection-auto-re-nudge-for-fleet-agents-herd-shoot-herd-spin`
**Role**: worker
**Agent**: opencode / litellm/lazer/deepseek-v4-flash
**Assigned Worktree**: .worktrees/worker

## Description

## Outcome
The forge detects a builder that has stalled — menu-blocked, spinning on the model with no output, or done with ZERO real commits — and auto-recovers: firm re-nudge via herd send, or herd shoot to interrupt, or re-dispatch. Root problem observed 2026-08-02: deepseek fleet agents report done with 0 real commits (only anchor/wip), stuck on the model spinner. Ports chainseer herd-spin (FAC-90) + herd-shoot (FAC-88).

## Acceptance
- [ ] detects done-but-no-commit and menu-stall states
- [ ] auto re-nudges/interrupts and re-drives to real completion
- [ ] go test ./pkg/process/

## Workflow

1. Enter worktree: `cd .herd/worktrees/fac-109`
2. Inspect existing code and understand what needs to change
3. Write failing tests first (TDD)
4. Implement the minimal solution
5. Verify: `go test ./...` (or equivalent test command)
6. Commit with a conventional commit message
7. Signal completion by moving the card to 'in-progress' (review pipeline)

## Role Context

Role prompt from: `.herd/prompts/worker.md`

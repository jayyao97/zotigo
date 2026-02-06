# Gemini CLI Agent Guidelines

This document outlines the operational protocols and behavioral standards for the Gemini CLI agent working on the Zotigo project.

## 📋 Task Management Workflow

### 1. Task Execution Cycle
1.  **Read Task**: Identify the current task from `task.json`.
2.  **Plan & Implement**: Execute the necessary code changes, adhering to the architecture defined in `README.md`.
3.  **Unit Testing**: Create accompanying `_test.go` files for logic-heavy components. Ensure tests cover key scenarios.
4.  **Build Verification**: Run `go build ./...` to ensure no compilation errors exist.
5.  **Self-Verification (Critical)**: Before declaring a task complete, explicitly check against the `acceptance_criteria` defined in `task.json`.
    *   Are all structs/functions implemented?
    *   Does the code compile?
    *   Are unit tests passing (`go test ./...`)?
6.  **Request Confirmation**: Update the task's status in `task.json` from `pending` -> `waiting_confirm`. Report this to the user and wait for approval.
7.  **Finalize**: Only after user explicitly approves (says "OK"), update status to `finish`.

### 2. Code Quality Standards
*   **English Only**: All code comments, variable names, and file contents must be in English.
*   **Communication**: All conversation with the user must be in **Chinese**.
*   **Idiomatic Go**: Follow standard Go conventions (e.g., `gofmt`, effective go patterns).
*   **Decoupling**: Strictly adhere to the 3-layer architecture (Presentation -> Protocol -> Provider).

### 3. Verification Checklist (Example)
When completing a task like "Define Conversation Protocol":
- [ ] Check if all files (`types.go`, `message.go`) exist.
- [ ] Verify `Message`, `Role`, `ToolCall` structs match the spec.
- [ ] Run `go mod tidy` to ensure no broken dependencies.
- [ ] Run `go build ./...` to confirm compilation success.
- [ ] Run `go test ./...` to confirm logic correctness.

## 🛠️ Project Context
*   **Current Phase**: Core Architecture Reconstruction.
*   **Goal**: Build a universal Agent CLI supporting OpenAI, Claude, and Gemini.
*   **Key Files**:
    *   `core/protocol`: The universal data model.
    *   `core/config`: Configuration management.
    *   `core/providers`: LLM adapters.

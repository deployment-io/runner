# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Runner is an agent that executes DevOps and infrastructure tasks. It can be installed locally or on cloud environments to automate CI/CD, builds, deployments, and cloud resource management.

## Build Commands

```bash
# Install dependencies
go mod tidy

# Build the runner binary
go build -o deployment-runner ./...

# Run directly
go run ./...

# Run tests
go test ./...
```

## After Making Changes

Always run the following commands after making changes to any file in this project:

```bash
go build ./...
go test ./...
```

### Usage

```bash
TargetCloud=aws UserSecret=yourUserSecret UserKey=yourUserKey deployment-runner
```

## Architecture

### Design Principles

- **Command Pattern**: Tasks are implemented as commands, making it easy to add new tasks without modifying existing ones
- **Composability**: Complex tasks are created by chaining simpler tasks
- **Reusability**: Simple tasks can be reused across complex tasks
- **Security**: Runner is the only component with access to source code and cloud credentials

### Project Structure

```
├── agent/        # Agent logic
├── automation/   # Automation workflows
├── client/       # RPC client for control plane communication
├── entrypoints/  # Container entrypoint scripts
├── jobs/         # Job definitions
└── utils/        # Utility functions
```

### Client Operations

The `client/` package provides operations for:
- Agents, Automations, Builds
- Certificates, Clusters, Deployments
- Jobs, Logs, Notifications
- OAuth, Ping, Previews, VPCs

### Dependencies (via go.work)

- `deployment-runner-kit` - Shared types and AWS utilities
- `team-ai` - AI-related functionality
- `langchaingo` - LangChain Go fork
- `moby` - Docker/Moby for container operations

### Key Libraries

- **aws-sdk-go-v2** - AWS services (EC2, ECS, ECR, S3, CloudFront, RDS, etc.)
- **docker/docker** - Docker client
- **go-git** - Git operations
- **nixpacks-go** - Nixpacks for builds
- **tree-sitter** - Code parsing
- **mongo-driver** - MongoDB

## Environment Configuration

Uses `.env` files loaded via godotenv.

## Go Coding Style Guide

These rules guide the generation of Go code that is simple, readable, and maintainable.

### 1. The Principle of Least Abstraction

Start with the simplest possible solution. Clarity over cleverness.

- **Rule 1.1: Default to a Single Function** - Solve the problem within a single function first. Do not create helper functions, new types, or new packages prematurely.
- **Rule 1.2: Justify Every Abstraction** - Before creating a new function, struct, or package, justify its existence based on function length, parameter count, or the Rule of Three.

### 2. Function Design and Granularity

- **Rule 2.1: Functions Do One Thing** - Every function should have a single, clear responsibility describable in one sentence.
- **Rule 2.2: Strict Function Length Limit** - Functions should rarely exceed 50 lines. Decompose longer functions into smaller, private helper functions in the same file.
- **Rule 2.3: Strict Parameter Limit** - Maximum four parameters. Group related parameters into a struct, or make the function a method on a struct holding shared state.
- **Rule 2.4: Return Values** - Return one or two values directly. Use a named struct for three or more related return values.

### 3. Duplication vs. Abstraction

- **Rule 3.1: The Rule of Three** - Only refactor duplicated code on its third appearance.
- **Rule 3.2: Verify True Duplication** - Confirm duplicated code represents the same core logic before refactoring. Similar-looking code handling different business rules should remain separate.

### 4. Package and Interface Philosophy

- **Rule 4.1: Packages Have a Singular Purpose** - A package should represent a single concept. Avoid generic "utility," "common," or "helpers" packages.
- **Rule 4.2: Interfaces are Defined by the Consumer** - Define small interfaces on the consumer side describing only the behavior required.
- **Rule 4.3: Keep Interfaces Small** - Interfaces should ideally have one method. More than three methods is a red flag.
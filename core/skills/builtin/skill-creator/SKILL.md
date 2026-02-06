---
name: skill-creator
description: Guide for creating effective skills. Use when users want to create a new skill or update an existing skill.
aliases:
  - create-skill
  - new-skill
---

# Skill Creator Guide

You are helping the user create a new Zotigo skill. Skills extend the CLI's capabilities with specialized knowledge, workflows, or tool integrations.

## What is a Skill?

A skill is a reusable set of instructions that can be activated during a conversation. Skills allow you to:
- Define specialized behaviors for specific tasks
- Share domain expertise across projects
- Create consistent workflows

## Skill Directory Structure

```
skill-name/
├── SKILL.md          # Required: skill definition and instructions
├── scripts/          # Optional: helper scripts
├── references/       # Optional: reference documents
└── assets/           # Optional: templates, examples
```

## SKILL.md Format

Every skill requires a `SKILL.md` file with YAML front matter:

```markdown
---
name: my-skill
description: Brief description of what the skill does
aliases:
  - alias1
  - alias2
---

# Skill Title

Your detailed instructions go here...
```

### Required Fields
- `name`: Unique identifier (lowercase, hyphens allowed)
- `description`: Brief explanation (shown in `/skills` listing)

### Optional Fields
- `aliases`: Alternative names to activate the skill

## Skill Locations

Skills can be placed in two locations:

1. **User Skills** (`~/.zotigo/skills/`)
   - Available across all projects
   - Good for personal workflows

2. **Project Skills** (`.zotigo/skills/`)
   - Project-specific skills
   - Higher priority than user skills
   - Good for team-shared workflows

## Design Principles

### 1. Progressive Disclosure
- Keep the description brief
- Put detailed instructions in the SKILL.md body
- Use scripts for complex operations

### 2. Clear Instructions
- Be specific about expected behavior
- Include examples when helpful
- Define success criteria

### 3. Reusable Resources
- Create scripts for common operations
- Include reference documents for context
- Use templates for consistent outputs

## Creation Steps

1. **Understand Requirements**
   - What task should the skill help with?
   - What specialized knowledge is needed?
   - What should the output look like?

2. **Plan Structure**
   - Will you need scripts?
   - What references might help?
   - Are there templates to include?

3. **Create Skill Directory**
   ```bash
   mkdir -p ~/.zotigo/skills/my-skill
   # Or for project-specific:
   mkdir -p .zotigo/skills/my-skill
   ```

4. **Write SKILL.md**
   - Start with YAML front matter
   - Add clear instructions
   - Include examples

5. **Add Supporting Files** (Optional)
   - Scripts in `scripts/`
   - Reference docs in `references/`
   - Templates in `assets/`

6. **Test the Skill**
   ```
   /skills           # Verify skill appears
   /skill my-skill   # Activate and test
   ```

## Example Skills

### Code Review Skill
```markdown
---
name: code-review
description: Thorough code review with best practices
aliases:
  - review
  - cr
---

# Code Review Instructions

Perform a thorough code review focusing on:

1. **Correctness**: Logic errors, edge cases
2. **Security**: Input validation, vulnerabilities
3. **Performance**: Complexity, resource usage
4. **Readability**: Naming, structure, comments
5. **Maintainability**: Modularity, tests

Format your review as:
- Summary of changes
- Issues found (with severity)
- Suggestions for improvement
- What was done well
```

### Debug Skill
```markdown
---
name: debug
description: Systematic debugging approach
aliases:
  - d
---

# Debugging Instructions

Follow a systematic approach:

1. **Reproduce**: Confirm the issue
2. **Isolate**: Find the minimal case
3. **Identify**: Locate the root cause
4. **Fix**: Make targeted changes
5. **Verify**: Confirm the fix works
6. **Prevent**: Add tests if appropriate
```

## Tips

- Keep skill names short and memorable
- Use descriptive but concise descriptions
- Test skills in different scenarios
- Iterate based on actual usage
- Share useful skills with your team

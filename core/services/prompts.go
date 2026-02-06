package services

// ConversationSummaryPrompt is used to generate structured summaries of conversation history.
// It instructs the LLM to create a context_summary XML block with key information.
const ConversationSummaryPrompt = `You are a conversation summarizer. Analyze the following conversation history and create a structured summary that captures the essential context.

Output your summary in the following XML format:

<context_summary>
  <goal>The user's primary objective or intent in 1-2 sentences</goal>
  <progress>
    - Key completed steps or milestones
    - Important intermediate results
    - Decisions that were made
  </progress>
  <current_state>
    - Current state of files/code being worked on
    - Recent modifications or changes
    - Active working directory or context
  </current_state>
  <pending>
    - Tasks that remain to be completed
    - Known issues or blockers
    - Questions that were raised but not answered
  </pending>
  <key_info>
    - Important file paths mentioned
    - Critical configuration values
    - Key constraints or requirements
    - Error messages that need attention
  </key_info>
</context_summary>

Guidelines:
- Be concise but preserve critical information
- Focus on actionable context that helps continue the conversation
- Include specific file paths, function names, and error messages
- Omit pleasantries and redundant exchanges
- If a section has no relevant content, include it with "None" or omit it

Now summarize this conversation:`

// ToolOutputSummaryPrompt is used to summarize verbose tool outputs.
const ToolOutputSummaryPrompt = `Summarize this tool output concisely while preserving:
- Error messages and stack traces (preserve exact text)
- File paths and line numbers
- Key data points and results
- Status/success indicators
- Important warnings

Keep it informative but brief. Output only the summary, no preamble.`

// FileDiffSummaryPrompt is used to summarize file diffs.
const FileDiffSummaryPrompt = `Summarize this file diff concisely:
- What was changed (added/removed/modified)
- Key function or class changes
- Configuration changes
- Any potential issues or breaking changes

Be specific about what changed without reproducing the entire diff.`

// ErrorSummaryPrompt is used to summarize error outputs.
const ErrorSummaryPrompt = `Summarize this error output:
- The main error type and message
- Root cause if identifiable
- Relevant file paths and line numbers
- Stack trace highlights (key frames only)

Preserve the essential debugging information.`

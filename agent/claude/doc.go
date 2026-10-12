// Package claude runs the Claude Code CLI as an LLM backend: the in-process
// `claude -p` direct runner (bounded concurrency, tool-call emulation, retry on
// the degenerate zero-token envelope), the http.RoundTripper that exposes it as
// an OpenAI chat-completions endpoint, and the OAuth bearer round-tripper for
// Claude Code seats. It has no database or host dependencies; the host
// provides the runtime through agent/airuntime.
package claude

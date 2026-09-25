package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func sample() (session.Session, []message.Message) {
	sess := session.Session{ID: "s1", Title: "Fix the build", PromptTokens: 1200, CompletionTokens: 340, Cost: 0.01, CreatedAt: 1700000000, UpdatedAt: 1700000100}
	msgs := []message.Message{
		{ID: "m1", SessionID: "s1", Role: message.User, CreatedAt: 1700000000, Parts: []message.ContentPart{message.TextContent{Text: "make it green"}}},
		{ID: "m2", SessionID: "s1", Role: message.Assistant, Model: "qwen", Provider: "llmgw", CreatedAt: 1700000010, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "run tests first"},
			message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test ./..."}`},
			message.Finish{Reason: message.FinishReasonToolUse},
		}},
		{ID: "m3", SessionID: "s1", Role: message.Tool, CreatedAt: 1700000020, Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "c1", Name: "bash", Content: "ok", IsError: false},
		}},
	}
	return sess, msgs
}

func TestWriteJSONL(t *testing.T) {
	t.Parallel()
	sess, msgs := sample()
	var buf bytes.Buffer
	require.NoError(t, WriteJSONL(&buf, sess, msgs))

	var lines []map[string]any
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		var m map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &m), sc.Text())
		lines = append(lines, m)
	}
	require.Len(t, lines, 4, "header plus one line per message")
	require.Equal(t, "session", lines[0]["type"])
	require.Equal(t, "Fix the build", lines[0]["title"])
	require.Equal(t, float64(3), lines[0]["message_count"])

	require.Equal(t, "message", lines[2]["type"])
	require.Equal(t, "assistant", lines[2]["role"])
	parts := lines[2]["parts"].([]any)
	require.Len(t, parts, 3)
	call := parts[1].(map[string]any)
	require.Equal(t, "tool_call", call["type"])
	require.Equal(t, map[string]any{"command": "go test ./..."}, call["input"], "tool input is embedded as JSON, not a string")
	require.Equal(t, "tool_use", parts[2].(map[string]any)["reason"])
	require.Equal(t, "ok", lines[3]["parts"].([]any)[0].(map[string]any)["content"])
}

func TestWriteMarkdown(t *testing.T) {
	t.Parallel()
	sess, msgs := sample()
	var buf bytes.Buffer
	require.NoError(t, WriteMarkdown(&buf, sess, msgs))
	out := buf.String()
	require.True(t, strings.HasPrefix(out, "# Fix the build\n"))
	require.Contains(t, out, "1,200 tokens in, 340 tokens out")
	require.Contains(t, out, "## User ·")
	require.Contains(t, out, "## Assistant · qwen ·")
	require.Contains(t, out, "<details><summary>Thinking</summary>")
	require.Contains(t, out, "**Tool call** `bash`")
	require.Contains(t, out, "**Tool result** `bash`")
	require.Contains(t, out, "_(finished: tool_use)_")
}

type stubSessions struct{ s session.Session }

func (f stubSessions) Get(_ context.Context, id string) (session.Session, error) { return f.s, nil }

type stubMessages struct{ m []message.Message }

func (f stubMessages) List(_ context.Context, _ string) ([]message.Message, error) { return f.m, nil }

func TestWriteFileIsAtomicAndAtTheExpectedPath(t *testing.T) {
	t.Parallel()
	sess, msgs := sample()
	dir := t.TempDir()
	path, err := WriteFile(context.Background(), dir, stubSessions{sess}, stubMessages{msgs}, "s1")
	require.NoError(t, err)
	require.Equal(t, Path(dir, "s1"), path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 4, strings.Count(string(data), "\n"))
	entries, err := os.ReadDir(dir + "/transcripts")
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file left behind")
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Format{"": FormatJSONL, "jsonl": FormatJSONL, "md": FormatMarkdown, "Markdown": FormatMarkdown} {
		got, err := ParseFormat(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := ParseFormat("pdf")
	require.Error(t, err)
}

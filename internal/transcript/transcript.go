// Package transcript renders a session as a file: JSON Lines for tools and
// hooks, Markdown for people. The JSONL shape mirrors what Claude Code
// hands its hooks as transcript_path, one JSON object per line, so a hook
// or script written against that format reads a Crush transcript as well.
package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// Format names a transcript rendering.
type Format string

const (
	FormatJSONL    Format = "jsonl"
	FormatMarkdown Format = "md"
)

// ParseFormat accepts the format names the CLI takes.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "jsonl":
		return FormatJSONL, nil
	case "md", "markdown":
		return FormatMarkdown, nil
	}
	return "", fmt.Errorf("unknown transcript format %q (jsonl or md)", s)
}

// Header is the first JSONL line: the session itself.
type Header struct {
	Type             string  `json:"type"`
	SessionID        string  `json:"session_id"`
	Title            string  `json:"title"`
	ParentSessionID  string  `json:"parent_session_id,omitempty"`
	Created          string  `json:"created"`
	Modified         string  `json:"modified"`
	Cost             float64 `json:"cost"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	MessageCount     int     `json:"message_count"`
}

// Entry is one message line.
type Entry struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Created   string `json:"created"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Summary   bool   `json:"is_summary,omitempty"`
	Parts     []Part `json:"parts"`
}

// Part is one content part, tagged by type. Fields are set per type.
type Part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Hidden     bool            `json:"hidden,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Content    string          `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	MIMEType   string          `json:"mime_type,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Message    string          `json:"message,omitempty"`
}

// Source is what a transcript is rendered from.
type Source interface {
	Get(ctx context.Context, id string) (session.Session, error)
}

// MessageSource lists a session's messages.
type MessageSource interface {
	List(ctx context.Context, sessionID string) ([]message.Message, error)
}

// Path is where a session's JSONL transcript lives under the data
// directory, which is what hooks receive as transcript_path.
func Path(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "transcripts", sessionID+".jsonl")
}

// WriteFile renders the session to Path(dataDir, sessionID) and returns
// the path. The file is replaced atomically, so a hook reading it never
// sees a partial write.
func WriteFile(ctx context.Context, dataDir string, sessions Source, messages MessageSource, sessionID string) (string, error) {
	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("transcript: session %s: %w", sessionID, err)
	}
	msgs, err := messages.List(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("transcript: messages for %s: %w", sessionID, err)
	}
	path := Path(dataDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+sessionID+".*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := WriteJSONL(tmp, sess, msgs); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// Write renders the session in the given format.
func Write(w io.Writer, format Format, sess session.Session, msgs []message.Message) error {
	switch format {
	case FormatMarkdown:
		return WriteMarkdown(w, sess, msgs)
	default:
		return WriteJSONL(w, sess, msgs)
	}
}

// WriteJSONL writes the header line followed by one line per message.
func WriteJSONL(w io.Writer, sess session.Session, msgs []message.Message) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(header(sess, len(msgs))); err != nil {
		return err
	}
	for i := range msgs {
		if err := enc.Encode(entry(&msgs[i])); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// WriteMarkdown writes a readable transcript: a title block, then each
// message under a role heading with tool calls and results folded in.
func WriteMarkdown(w io.Writer, sess session.Session, msgs []message.Message) error {
	bw := bufio.NewWriter(w)
	title := sess.Title
	if title == "" {
		title = sess.ID
	}
	fmt.Fprintf(bw, "# %s\n\n", title)
	fmt.Fprintf(bw, "Session `%s`, %d messages, %s tokens in, %s tokens out.\n\n",
		sess.ID, len(msgs), groupThousands(sess.PromptTokens), groupThousands(sess.CompletionTokens))
	for i := range msgs {
		m := &msgs[i]
		who := strings.ToUpper(string(m.Role)[:1]) + string(m.Role)[1:]
		when := time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04:05")
		if m.Model != "" {
			fmt.Fprintf(bw, "## %s · %s · %s\n\n", who, m.Model, when)
		} else {
			fmt.Fprintf(bw, "## %s · %s\n\n", who, when)
		}
		for _, p := range m.Parts {
			switch v := p.(type) {
			case message.TextContent:
				if v.Hidden {
					fmt.Fprintf(bw, "_(hidden continuation)_\n\n")
				}
				fmt.Fprintf(bw, "%s\n\n", strings.TrimSpace(v.Text))
			case message.ReasoningContent:
				if strings.TrimSpace(v.Thinking) != "" {
					fmt.Fprintf(bw, "<details><summary>Thinking</summary>\n\n%s\n\n</details>\n\n", strings.TrimSpace(v.Thinking))
				}
			case message.ToolCall:
				fmt.Fprintf(bw, "**Tool call** `%s`\n\n```json\n%s\n```\n\n", v.Name, strings.TrimSpace(v.Input))
			case message.ToolResult:
				label := "Tool result"
				if v.IsError {
					label = "Tool error"
				}
				fmt.Fprintf(bw, "**%s** `%s`\n\n```\n%s\n```\n\n", label, v.Name, strings.TrimSpace(v.Content))
			case message.Finish:
				if v.Reason != "" && v.Reason != message.FinishReasonEndTurn {
					fmt.Fprintf(bw, "_(finished: %s)_\n\n", v.Reason)
				}
			}
		}
	}
	return bw.Flush()
}

func header(sess session.Session, n int) Header {
	return Header{
		Type:             "session",
		SessionID:        sess.ID,
		Title:            sess.Title,
		ParentSessionID:  sess.ParentSessionID,
		Created:          time.Unix(sess.CreatedAt, 0).UTC().Format(time.RFC3339),
		Modified:         time.Unix(sess.UpdatedAt, 0).UTC().Format(time.RFC3339),
		Cost:             sess.Cost,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		MessageCount:     n,
	}
}

func entry(m *message.Message) Entry {
	e := Entry{
		Type:      "message",
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      string(m.Role),
		Created:   time.Unix(m.CreatedAt, 0).UTC().Format(time.RFC3339),
		Model:     m.Model,
		Provider:  m.Provider,
		Summary:   m.IsSummaryMessage,
		Parts:     make([]Part, 0, len(m.Parts)),
	}
	for _, p := range m.Parts {
		switch v := p.(type) {
		case message.TextContent:
			e.Parts = append(e.Parts, Part{Type: "text", Text: v.Text, Hidden: v.Hidden})
		case message.ReasoningContent:
			e.Parts = append(e.Parts, Part{Type: "reasoning", Thinking: v.Thinking})
		case message.ToolCall:
			input := json.RawMessage(v.Input)
			if !json.Valid(input) {
				input, _ = json.Marshal(v.Input)
			}
			e.Parts = append(e.Parts, Part{Type: "tool_call", ToolCallID: v.ID, Name: v.Name, Input: input})
		case message.ToolResult:
			e.Parts = append(e.Parts, Part{Type: "tool_result", ToolCallID: v.ToolCallID, Name: v.Name, Content: v.Content, IsError: v.IsError, MIMEType: v.MIMEType})
		case message.BinaryContent:
			e.Parts = append(e.Parts, Part{Type: "binary", MIMEType: v.MIMEType})
		case message.ImageURLContent:
			e.Parts = append(e.Parts, Part{Type: "image_url", Content: v.URL})
		case message.Finish:
			e.Parts = append(e.Parts, Part{Type: "finish", Reason: string(v.Reason), Message: v.Message})
		}
	}
	return e
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

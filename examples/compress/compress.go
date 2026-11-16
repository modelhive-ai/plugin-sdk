// Package plugincompress is a small request compressor plugin.
//
// It shrinks tool output with RedactSpans: runs of identical lines, long runs
// of blank lines and spaces, and outputs so long that only the head and the
// tail are worth keeping. Importing the package registers it for in-process
// use; cmd/plugin-compress serves the same implementation out of process.
package plugincompress

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelhive-ai/plugin-sdk/pkg/plugin"
)

const (
	// ID is the identifier used for in-process registration.
	ID = "compress"

	// AnnotationBytesSaved is the annotation carrying the bytes removed.
	AnnotationBytesSaved = "compress.bytes_saved"

	// The gateway refuses a request whose span keys exceed 4096 bytes and
	// accepts 4096 spans in total, shared by every instance.
	maxSpanBytes = 4096
	maxSpans     = 1024
	// chunkBytes keeps a chunk plus UTF-8 alignment under maxSpanBytes.
	chunkBytes = 4000
	// minOmitted keeps a trimmed key long enough to be unique in the body.
	minOmitted = 256

	blankRunMin = 4
	spaceRunMin = 16
)

// Config tunes one instance. Zero or absent fields take the defaults.
type Config struct {
	// MinRepeat is how many identical consecutive lines collapse.
	MinRepeat int `json:"min_repeat"`
	// MaxToolBytes is the size above which an output is cut to head and tail.
	MaxToolBytes int `json:"max_tool_bytes"`
	HeadBytes    int `json:"head_bytes"`
	TailBytes    int `json:"tail_bytes"`
}

func defaultConfig() Config {
	return Config{MinRepeat: 3, MaxToolBytes: 16384, HeadBytes: 4096, TailBytes: 2048}
}

func (c Config) validate() error {
	switch {
	case c.MinRepeat < 2:
		return fmt.Errorf("compress config: min_repeat must be at least 2, got %d", c.MinRepeat)
	case c.HeadBytes < 1 || c.TailBytes < 1:
		return fmt.Errorf("compress config: head_bytes and tail_bytes must be positive")
	case c.HeadBytes+c.TailBytes >= c.MaxToolBytes:
		return fmt.Errorf("compress config: head_bytes + tail_bytes must be below max_tool_bytes")
	}
	return nil
}

// Plugin compresses tool output in every request it sees.
type Plugin struct {
	cfg Config
}

// New builds a compress plugin for one configured instance.
func New(_ context.Context, in plugin.Instance) (plugin.Handler, error) {
	cfg := defaultConfig()
	if in.Config != nil {
		raw, err := json.Marshal(in.Config)
		if err != nil {
			return nil, fmt.Errorf("encode compress config: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode compress config: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Plugin{cfg: cfg}, nil
}

// Process derives its spans only from the content of each tool output, so a
// turn compresses the same way in every request that carries it and the
// provider's prompt-cache prefix keeps matching.
func (p *Plugin) Process(ctx context.Context, r *plugin.Request) (plugin.Ops, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Ops{}, err
	}
	spans := map[string]string{}
	for i := range r.Messages {
		if err := ctx.Err(); err != nil {
			return plugin.Ops{}, err
		}
		for _, text := range toolOutputs(&r.Messages[i]) {
			p.shrink(spans, text)
		}
	}
	if len(spans) == 0 {
		return plugin.Ops{}, nil
	}
	saved := bytesSaved(r, spans)
	if saved <= 0 {
		return plugin.Ops{}, nil
	}
	return plugin.Ops{
		RedactSpans: spans,
		Annotations: map[string]string{AnnotationBytesSaved: strconv.Itoa(saved)},
	}, nil
}

// toolOutputs returns the tool output of a message: ToolText on a user turn,
// Text on a "tool" turn.
func toolOutputs(m *plugin.Message) []string {
	var out []string
	if m.ToolText != "" {
		out = append(out, m.ToolText)
	}
	if m.Role == "tool" && m.Text != "" {
		out = append(out, m.Text)
	}
	return out
}

func (p *Plugin) shrink(spans map[string]string, text string) {
	if len(text) > p.cfg.MaxToolBytes {
		headEnd := cutAfterHead(text, p.cfg.HeadBytes)
		tailStart := cutBeforeTail(text, p.cfg.TailBytes)
		if middle := text[headEnd:tailStart]; len(middle) >= minOmitted && trim(spans, middle) {
			collapse(spans, text[:headEnd], p.cfg.MinRepeat)
			collapse(spans, text[tailStart:], p.cfg.MinRepeat)
			return
		}
	}
	collapse(spans, text, p.cfg.MinRepeat)
}

// cutAfterHead returns where the kept head ends: after its last full line.
func cutAfterHead(text string, n int) int {
	if i := strings.LastIndexByte(text[:n], '\n'); i >= 0 {
		return i + 1
	}
	return runeStartBefore(text, n)
}

// cutBeforeTail returns where the kept tail starts: after a line break.
func cutBeforeTail(text string, n int) int {
	from := len(text) - n
	if i := strings.IndexByte(text[from:], '\n'); i >= 0 && from+i+1 < len(text) {
		return from + i + 1
	}
	return runeStartAfter(text, from)
}

func runeStartBefore(s string, i int) int {
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func runeStartAfter(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// trim removes middle in key-sized chunks: the first becomes a marker, the
// rest vanish. It reports false, adding nothing, when a chunk cannot be
// expressed safely.
func trim(spans map[string]string, middle string) bool {
	n := (len(middle) + chunkBytes - 1) / chunkBytes
	marker := fmt.Sprintf("[... %d bytes omitted ...]\n", len(middle))
	local := map[string]string{}
	start := 0
	for k := 1; k <= n; k++ {
		end := len(middle)
		if k < n {
			end = runeStartAfter(middle, k*len(middle)/n)
		}
		chunk, repl := middle[start:end], ""
		if k == 1 {
			repl = marker
		}
		if _, dup := local[chunk]; dup || !put(local, chunk, repl) {
			return false
		}
		start = end
	}
	fresh := 0
	for key, repl := range local {
		old, ok := spans[key]
		if ok && old != repl {
			return false
		}
		if !ok {
			fresh++
		}
	}
	if len(spans)+fresh > maxSpans {
		return false
	}
	for key, repl := range local {
		spans[key] = repl
	}
	return true
}

// collapse adds spans for long blank and space runs and for repeated lines.
func collapse(spans map[string]string, text string, minRepeat int) {
	runs(spans, text, '\n', blankRunMin, "\n\n")
	runs(spans, text, ' ', spaceRunMin, " ")
	lines := strings.SplitAfter(text, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]
		j := i + 1
		for j < len(lines) && lines[j] == line {
			j++
		}
		n := j - i
		i = j
		if n < minRepeat || !strings.HasSuffix(line, "\n") || strings.TrimSpace(line) == "" {
			continue
		}
		if len(line)*minRepeat > maxSpanBytes {
			continue
		}
		n = min(n, maxSpanBytes/len(line))
		put(spans, strings.Repeat(line, n), fmt.Sprintf("%s[repeated %d more times]\n", line, n-1))
	}
}

// runs adds a span for each maximal run of c of at least minLen bytes.
func runs(spans map[string]string, text string, c byte, minLen int, repl string) {
	for i := 0; i < len(text); {
		if text[i] != c {
			i++
			continue
		}
		j := i
		for j < len(text) && text[j] == c {
			j++
		}
		if j-i >= minLen {
			put(spans, text[i:min(j, i+maxSpanBytes)], repl)
		}
		i = j
	}
}

// put adds a span the gateway will accept and that shrinks the text. It
// reports false when the span was refused or conflicts with an existing one.
func put(spans map[string]string, key, repl string) bool {
	if old, ok := spans[key]; ok {
		return old == repl
	}
	switch {
	case key == "" || len(key) > maxSpanBytes || len(repl) >= len(key),
		strings.Contains(repl, key), !utf8.ValidString(key),
		len(spans) >= maxSpans:
		return false
	}
	spans[key] = repl
	return true
}

// bytesSaved applies spans the way the gateway does, to the message text and
// tool output, and returns how many bytes that removes.
func bytesSaved(r *plugin.Request, spans map[string]string) int {
	keys := orderedKeys(spans)
	saved := 0
	for _, m := range r.Messages {
		saved += len(m.Text) - len(applySpans(m.Text, keys, spans))
		saved += len(m.ToolText) - len(applySpans(m.ToolText, keys, spans))
	}
	return saved
}

// orderedKeys sorts keys longest first, then alphabetically: the gateway's
// order, which decides the result when spans overlap.
func orderedKeys(spans map[string]string) []string {
	keys := make([]string, 0, len(spans))
	for key := range spans {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

func applySpans(text string, keys []string, spans map[string]string) string {
	for _, key := range keys {
		text = strings.ReplaceAll(text, key, spans[key])
	}
	return text
}

// Definition is registered in-process and served by the standalone command.
var Definition = plugin.Definition{
	Ref: ID,
	New: New,
}

func init() {
	plugin.Register(Definition)
}

var _ plugin.Processor = (*Plugin)(nil)

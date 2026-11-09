package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/on-the-ground/muster/internal/contract"
	"github.com/on-the-ground/muster/internal/strictjson"
)

// Effect is one native tool use the worker requested, as the CLI's event
// stream reported it. Status is "requested" until the CLI reports a result
// ("ok" or "error"), or "denied" when native permissions refused it. A
// "requested" effect at the end of a job may still have happened remotely.
type Effect struct {
	Tool        string     `json:"tool"`
	Target      string     `json:"target,omitempty"`
	InputHash   string     `json:"inputHash"`
	RequestedAt time.Time  `json:"requestedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Status      string     `json:"status"`
	ResultHash  string     `json:"resultHash,omitempty"`
	// ObservedURLs are URLs found in the tool result text. The extraction is a
	// heuristic over CLI output, useful for checking citations, not a record of
	// every page the remote service read.
	ObservedURLs []string `json:"observedUrls,omitempty"`
}

// ErrPolicy reports that the worker exposed or used a tool outside its
// declared permissions. The job is stopped when it is observed.
var ErrPolicy = errors.New("worker used a tool outside its declared permissions")

// ErrAuditLimit reports that the worker requested more tool uses than one
// job's evidence records. The job is stopped when the first unrecorded use is
// observed, and its effect audit is incomplete.
var ErrAuditLimit = errors.New("worker exceeded the effect audit limit")

const (
	maxTarget       = 2048
	maxObservedURLs = 50
	maxEffects      = 256
)

// structuredOutputTool is how the CLI returns the --json-schema result. It
// is the extraction channel, not an effect.
const structuredOutputTool = "StructuredOutput"

var urlPattern = regexp.MustCompile(`https?://[^\s"'<>\\)\]]+`)

// streamParser reads Claude stream-json events line by line as they arrive,
// so effects carry the time muster observed them and an undeclared tool stops
// the job at once. It keeps only what extraction and evidence need.
type streamParser struct {
	mu        sync.Mutex
	allowed   map[string]bool
	onStop    func(reason string)
	partial   []byte
	effects   []Effect
	byID      map[string]int
	session   string
	result    map[string]json.RawMessage
	denied    []string
	violation bool
	overflow  bool
	malformed bool
	now       func() time.Time
}

// onStop is called once, with "policy" or "audit-limit", when the job must be
// stopped.
func newStreamParser(tools []string, onStop func(reason string)) *streamParser {
	allowed := map[string]bool{structuredOutputTool: true}
	for _, t := range tools {
		allowed[t] = true
	}
	return &streamParser{allowed: allowed, onStop: onStop, byID: map[string]int{}, now: func() time.Time { return time.Now().UTC() }}
}

func (s *streamParser) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, data...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := s.partial[:i]
		s.partial = s.partial[i+1:]
		s.line(line)
	}
	if len(s.partial) > strictjson.MaxBytes {
		// One event larger than the frame limit cannot be validated.
		s.malformed = true
		s.partial = nil
		return 0, errors.New("worker stream event exceeds 4 MiB")
	}
	return len(data), nil
}

func (s *streamParser) line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	m, err := strictjson.Fields(line)
	if err != nil {
		s.malformed = true
		return
	}
	var kind, subtype string
	_ = json.Unmarshal(m["type"], &kind)
	_ = json.Unmarshal(m["subtype"], &subtype)
	var sid string
	if json.Unmarshal(m["session_id"], &sid) == nil && s.session == "" {
		s.session = safeID(sid)
	}
	switch {
	case kind == "system" && subtype == "init":
		var tools []string
		if json.Unmarshal(m["tools"], &tools) != nil {
			s.malformed = true
			return
		}
		for _, t := range tools {
			if !s.allowed[t] {
				s.violate()
			}
		}
	case kind == "assistant" || kind == "user":
		var msg struct {
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m["message"], &msg) != nil {
			return
		}
		for _, raw := range msg.Content {
			s.block(raw)
		}
	case kind == "result":
		s.result = m
		var denials []struct {
			ToolName  string `json:"tool_name"`
			ToolUseID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(m["permission_denials"], &denials)
		for _, d := range denials {
			s.denied = append(s.denied, d.ToolName)
			if i, ok := s.byID[d.ToolUseID]; ok {
				s.effects[i].Status = "denied"
			}
		}
	}
}

func (s *streamParser) block(raw json.RawMessage) {
	var b struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return
	}
	switch b.Type {
	case "tool_use", "server_tool_use":
		if !s.allowed[b.Name] {
			s.violate()
		}
		if b.Name == structuredOutputTool {
			return
		}
		if len(s.effects) >= maxEffects {
			// This use cannot be recorded: stop the job rather than let
			// unaudited requests continue.
			if !s.overflow {
				s.overflow = true
				s.stop("audit-limit")
			}
			return
		}
		e := Effect{Tool: b.Name, InputHash: contract.Hash(b.Input), RequestedAt: s.now(), Status: "requested"}
		var input struct {
			URL   string `json:"url"`
			Query string `json:"query"`
		}
		_ = json.Unmarshal(b.Input, &input)
		e.Target = input.URL
		if e.Target == "" {
			e.Target = input.Query
		}
		if len(e.Target) > maxTarget {
			e.Target = e.Target[:maxTarget]
		}
		s.byID[b.ID] = len(s.effects)
		s.effects = append(s.effects, e)
	case "tool_result":
		i, ok := s.byID[b.ToolUseID]
		if !ok {
			return
		}
		now := s.now()
		e := &s.effects[i]
		e.CompletedAt, e.ResultHash, e.Status = &now, contract.Hash(b.Content), "ok"
		if b.IsError {
			e.Status = "error"
		}
		text := b.Content
		var str string
		if json.Unmarshal(b.Content, &str) == nil {
			text = []byte(str)
		}
		seen := map[string]bool{}
		for _, u := range urlPattern.FindAllString(string(text), -1) {
			if len(e.ObservedURLs) >= maxObservedURLs {
				break
			}
			if len(u) <= maxTarget && !seen[u] {
				seen[u] = true
				e.ObservedURLs = append(e.ObservedURLs, u)
			}
		}
	}
}

func (s *streamParser) violate() {
	if !s.violation {
		s.violation = true
		s.stop("policy")
	}
}

// stop asks for the job to end. It runs outside the parser lock because
// cancellation takes the process lock.
func (s *streamParser) stop(reason string) {
	if s.onStop != nil {
		go s.onStop(reason)
	}
}

// auditIncomplete reports whether tool uses went unrecorded.
func (s *streamParser) auditIncomplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflow
}

// outcome returns the extracted structured output and evidence. Effects are
// returned even when extraction fails, because they may have happened.
func (s *streamParser) outcome() (json.RawMessage, string, []Effect, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	effects := append([]Effect(nil), s.effects...)
	denied := append([]string(nil), s.denied...)
	switch {
	case s.violation:
		return nil, s.session, effects, denied, ErrPolicy
	case s.overflow:
		return nil, s.session, effects, denied, ErrAuditLimit
	case s.malformed || s.result == nil:
		return nil, s.session, effects, denied, ErrOutput
	}
	var failed bool
	if raw, ok := s.result["is_error"]; !ok || json.Unmarshal(raw, &failed) != nil || failed {
		return nil, s.session, effects, denied, errors.New("worker reported execution failure")
	}
	output, ok := s.result["structured_output"]
	if !ok {
		return nil, s.session, effects, denied, ErrOutput
	}
	return output, s.session, effects, denied, nil
}

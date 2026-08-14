package agent

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/protocol"
)

var ErrHistoryRecord = errors.New("record durable history")

// HistoryMutation is a durable conversation change. Recorders must persist it
// synchronously and must not call back into Agent while RecordHistory runs.
type HistoryMutation struct {
	Replace             bool                     `json:"replace,omitempty"`
	Messages            []protocol.Message       `json:"messages"`
	UserContextState    *prompt.UserContextState `json:"user_context_state,omitempty"`
	HasUserContextState bool                     `json:"has_user_context_state,omitempty"`
}

// HistoryRecorder makes conversation state durable before memory advances.
type HistoryRecorder interface {
	RecordHistory(HistoryMutation) error
}

// DurabilityRecorder covers both model-visible conversation changes and the
// boundary before a tool may produce side effects.
type DurabilityRecorder interface {
	HistoryRecorder
	ToolExecutionRecorder
}

// conversationState owns the model-visible history and the cursor describing
// which contextual messages it contains. Agent.mu guards every method.
type conversationState struct {
	// history is read-only outside this file. Mutations must use append,
	// replace, or restore so durable recording cannot be bypassed.
	history          []protocol.Message
	userContextState *prompt.UserContextState
	recorder         HistoryRecorder
}

func newConversationState() conversationState {
	return conversationState{history: make([]protocol.Message, 0)}
}

// WithHistoryRecorder installs the synchronous durability boundary used by
// runtimes that need mid-turn crash recovery.
func WithHistoryRecorder(recorder HistoryRecorder) AgentOption {
	return func(a *Agent) { a.conversation.recorder = recorder }
}

// WithDurabilityRecorder installs every runtime durability boundary together
// so a host cannot journal history while accidentally omitting tool starts.
func WithDurabilityRecorder(recorder DurabilityRecorder) AgentOption {
	return func(a *Agent) {
		a.conversation.recorder = recorder
		a.toolExecutionRecorder = recorder
	}
}

func (s *conversationState) append(messages []protocol.Message, userContextState *prompt.UserContextState, hasUserContextState bool) error {
	return s.apply(HistoryMutation{
		Messages:            cloneMessages(messages),
		UserContextState:    userContextState,
		HasUserContextState: hasUserContextState,
	})
}

func (s *conversationState) appendMessages(messages ...protocol.Message) error {
	if len(messages) == 0 {
		return nil
	}
	return s.append(messages, nil, false)
}

func (s *conversationState) replace(messages []protocol.Message, userContextState *prompt.UserContextState) error {
	return s.apply(HistoryMutation{
		Replace:             true,
		Messages:            cloneMessages(messages),
		UserContextState:    userContextState,
		HasUserContextState: true,
	})
}

func (s *conversationState) apply(mutation HistoryMutation) error {
	if s.recorder != nil {
		if err := s.recorder.RecordHistory(cloneHistoryMutation(mutation)); err != nil {
			return fmt.Errorf("%w: %v", ErrHistoryRecord, err)
		}
	}
	if mutation.Replace {
		s.history = mutation.Messages
	} else {
		s.history = append(s.history, mutation.Messages...)
	}
	if mutation.HasUserContextState {
		s.userContextState = mutation.UserContextState.Clone()
	}
	return nil
}

// restore is the one non-journaled write path. It is only used when adopting a
// snapshot that is already durable.
func (s *conversationState) restore(messages []protocol.Message, userContextState *prompt.UserContextState) {
	s.history = cloneMessages(messages)
	s.userContextState = userContextState.Clone()
}

func (s *conversationState) snapshot() ([]protocol.Message, *prompt.UserContextState) {
	messages := cloneMessages(s.history)
	return messages, s.userContextState.Clone()
}

func cloneHistoryMutation(mutation HistoryMutation) HistoryMutation {
	mutation.Messages = cloneMessages(mutation.Messages)
	mutation.UserContextState = mutation.UserContextState.Clone()
	return mutation
}

func cloneMessages(messages []protocol.Message) []protocol.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]protocol.Message, len(messages))
	for i := range messages {
		cloned[i] = cloneMessage(messages[i])
	}
	return cloned
}

func cloneMessage(message protocol.Message) protocol.Message {
	message.Content = cloneContentParts(message.Content)
	if message.Metadata != nil {
		metadata := *message.Metadata
		if metadata.Usage != nil {
			usage := *metadata.Usage
			metadata.Usage = &usage
		}
		if metadata.ToolUsage != nil {
			usage := *metadata.ToolUsage
			metadata.ToolUsage = &usage
		}
		metadata.Raw = cloneStringAnyMap(metadata.Raw)
		message.Metadata = &metadata
	}
	if message.FinishedAt != nil {
		finishedAt := *message.FinishedAt
		message.FinishedAt = &finishedAt
	}
	return message
}

func cloneContentParts(parts []protocol.ContentPart) []protocol.ContentPart {
	if parts == nil {
		return nil
	}
	cloned := make([]protocol.ContentPart, len(parts))
	for i, part := range parts {
		part.Image = cloneMediaPart(part.Image)
		part.Audio = cloneMediaPart(part.Audio)
		part.Video = cloneMediaPart(part.Video)
		if part.File != nil {
			file := *part.File
			file.Data = append([]byte(nil), part.File.Data...)
			part.File = &file
		}
		if part.ToolCall != nil {
			toolCall := *part.ToolCall
			part.ToolCall = &toolCall
		}
		if part.ToolResult != nil {
			toolResult := *part.ToolResult
			toolResult.JSON = cloneJSONValue(toolResult.JSON)
			toolResult.Metadata = cloneStringAnyMap(toolResult.Metadata)
			if toolResult.Content != nil {
				toolResult.Content = append([]protocol.ToolResultContentPart(nil), toolResult.Content...)
				for j := range toolResult.Content {
					toolResult.Content[j].Image = cloneMediaPart(toolResult.Content[j].Image)
				}
			}
			part.ToolResult = &toolResult
		}
		cloned[i] = part
	}
	return cloned
}

func cloneMediaPart(part *protocol.MediaPart) *protocol.MediaPart {
	if part == nil {
		return nil
	}
	cloned := *part
	cloned.Data = append([]byte(nil), part.Data...)
	return &cloned
}

func cloneStringAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	if value == nil {
		return nil
	}
	cloned := cloneReflectValue(reflect.ValueOf(value), make(map[cloneVisit]reflect.Value))
	if !cloned.IsValid() {
		return value
	}
	return cloned.Interface()
}

type cloneVisit struct {
	typ reflect.Type
	ptr uintptr
	len int
	cap int
}

func cloneReflectValue(value reflect.Value, seen map[cloneVisit]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneReflectValue(value.Elem(), seen)
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if cloned, ok := seen[visit]; ok {
			return cloned
		}
		result := reflect.New(value.Type().Elem())
		seen[visit] = result
		result.Elem().Set(cloneReflectValue(value.Elem(), seen))
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if cloned, ok := seen[visit]; ok {
			return cloned
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = result
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneReflectValue(iterator.Key(), seen), cloneReflectValue(iterator.Value(), seen))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer(), len: value.Len(), cap: value.Cap()}
		if cloned, ok := seen[visit]; ok {
			return cloned
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		seen[visit] = result
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneReflectValue(value.Index(i), seen))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneReflectValue(value.Index(i), seen))
		}
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if result.Field(i).CanSet() && value.Type().Field(i).IsExported() {
				result.Field(i).Set(cloneReflectValue(value.Field(i), seen))
			}
		}
		return result
	default:
		return value
	}
}

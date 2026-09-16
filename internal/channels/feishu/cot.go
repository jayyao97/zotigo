package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/internal/channels"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	cotEndpoint        = "https://fsopen.bytedance.net/open-apis/im/v1/message_cot"
	cotRequestInterval = 65 * time.Millisecond
)

type cotAPI interface {
	Create(context.Context, channels.InboundMessage) (channels.DeliveryReceipt, error)
	Append(context.Context, channels.DeliveryReceipt, []*larkim.MessageCot) error
	Complete(context.Context, channels.DeliveryReceipt, string) error
}

type cotClient struct {
	do   func(context.Context, *larkcore.ApiReq) (*larkcore.ApiResp, error)
	pace func(context.Context) error
}

func newCOTClient(client *lark.Client) cotAPI {
	return &cotClient{
		do: func(ctx context.Context, req *larkcore.ApiReq) (*larkcore.ApiResp, error) {
			return client.Do(ctx, req)
		},
		// The COT API permits 50 requests/second and 1000 requests/minute.
		// The minute quota is tighter, so leave a small margin above 60 ms.
		pace: newRequestPacer(cotRequestInterval),
	}
}

func newRequestPacer(interval time.Duration) func(context.Context) error {
	var mu sync.Mutex
	var next time.Time
	return func(ctx context.Context) error {
		mu.Lock()
		now := time.Now()
		if next.Before(now) {
			next = now
		}
		wait := time.Until(next)
		next = next.Add(interval)
		mu.Unlock()
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}

type cotRejectedError struct {
	code int
	msg  string
}

func (e *cotRejectedError) Error() string {
	return fmt.Sprintf("COT request rejected (code %d): %s", e.code, e.msg)
}

type cotResponse struct {
	Code *int   `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		COTID     string `json:"cot_id"`
		MessageID string `json:"message_id"`
	} `json:"data"`
}

func (c *cotClient) request(ctx context.Context, req *larkcore.ApiReq) (cotResponse, error) {
	if c.pace != nil {
		if err := c.pace(ctx); err != nil {
			return cotResponse{}, fmt.Errorf("pace COT request: %w", err)
		}
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return cotResponse{}, fmt.Errorf("send COT request: %w", err)
	}
	if resp == nil {
		return cotResponse{}, errors.New("send COT request: response missing")
	}
	var body cotResponse
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = json.Unmarshal(resp.RawBody, &body)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusTooManyRequests && body.Code != nil && *body.Code != 0 {
			return cotResponse{}, &cotRejectedError{code: *body.Code, msg: body.Msg}
		}
		return cotResponse{}, fmt.Errorf("COT request returned HTTP status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(resp.RawBody, &body); err != nil {
		return cotResponse{}, fmt.Errorf("decode COT response: %w", err)
	}
	if body.Code == nil {
		return cotResponse{}, errors.New("decode COT response: response omitted code")
	}
	if *body.Code != 0 {
		// A successful HTTP status with a business error does not prove whether
		// the carrier was created. Keep it out of the definite-rejection path so
		// auto mode cannot create a second progress carrier.
		return cotResponse{}, fmt.Errorf("COT request returned business code %d: %s", *body.Code, body.Msg)
	}
	return body, nil
}

func (c *cotClient) Create(ctx context.Context, in channels.InboundMessage) (channels.DeliveryReceipt, error) {
	body := map[string]any{
		"receive_id":        in.ChatID,
		"origin_message_id": in.MessageID,
		"reply_in_thread":   in.ReplyMode != channels.ReplyModeDirect,
		"enable_badge":      false,
		"update_feed_rank":  false,
		"cot_hidden":        false,
	}
	resp, err := c.request(ctx, &larkcore.ApiReq{
		HttpMethod:                http.MethodPost,
		ApiPath:                   cotEndpoint,
		Body:                      body,
		QueryParams:               larkcore.QueryParams{"receive_id_type": []string{"chat_id"}},
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
	})
	if err != nil {
		return channels.DeliveryReceipt{}, err
	}
	if resp.Data == nil || resp.Data.COTID == "" || resp.Data.MessageID == "" {
		return channels.DeliveryReceipt{}, errors.New("create COT: response omitted cot_id or message_id")
	}
	return channels.DeliveryReceipt{Mode: channels.ProgressModeCOT, ReplyMode: in.ReplyMode, MessageID: resp.Data.MessageID, COTID: resp.Data.COTID}, nil
}

func (c *cotClient) Append(ctx context.Context, receipt channels.DeliveryReceipt, events []*larkim.MessageCot) error {
	if len(events) == 0 {
		return nil
	}
	for start := 0; start < len(events); start += 50 {
		end := min(start+50, len(events))
		_, err := c.request(ctx, &larkcore.ApiReq{
			HttpMethod:                http.MethodPut,
			ApiPath:                   cotEndpoint,
			Body:                      map[string]any{"events": events[start:end], "message_id": receipt.MessageID, "cot_id": receipt.COTID},
			SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *cotClient) Complete(ctx context.Context, receipt channels.DeliveryReceipt, reason string) error {
	_, err := c.request(ctx, &larkcore.ApiReq{
		HttpMethod:                http.MethodPost,
		ApiPath:                   cotEndpoint + "/complete/:cot_id",
		PathParams:                larkcore.PathParams{"cot_id": receipt.COTID},
		QueryParams:               larkcore.QueryParams{"message_id": []string{receipt.MessageID}, "reason": []string{reason}},
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
	})
	return err
}

type cotProgressHandle struct {
	api        cotAPI
	receipt    channels.DeliveryReceipt
	originID   string
	finalReply func(context.Context, string, channels.TaskResult, string, bool) (string, error)
	runID      string
}

func openCOTProgress(ctx context.Context, api cotAPI, finalReply func(context.Context, string, channels.TaskResult, string, bool) (string, error), in channels.InboundMessage) (channels.ProgressHandle, error) {
	receipt, err := api.Create(ctx, in)
	if err != nil {
		return nil, err
	}
	return &cotProgressHandle{api: api, receipt: receipt, originID: in.MessageID, finalReply: finalReply, runID: "run-" + receipt.COTID}, nil
}

func resumeCOTProgress(api cotAPI, finalReply func(context.Context, string, channels.TaskResult, string, bool) (string, error), receipt channels.DeliveryReceipt) channels.ProgressHandle {
	return &cotProgressHandle{api: api, receipt: receipt, finalReply: finalReply, runID: "run-" + receipt.COTID}
}

func (h *cotProgressHandle) Receipt() channels.DeliveryReceipt { return h.receipt }

func (h *cotProgressHandle) Update(ctx context.Context, progress channels.Progress) error {
	events, err := renderCOTEvents(h.receipt.COTID, h.runID, progress.Events)
	if err != nil {
		return err
	}
	return h.api.Append(ctx, h.receipt, events)
}

func (h *cotProgressHandle) Complete(ctx context.Context, result channels.TaskResult, persistFinal func(string) error) error {
	if h.originID == "" {
		return errors.New("complete COT: origin message is unavailable")
	}
	finalID, err := h.finalReply(ctx, h.originID, result, "zotigo-final-"+h.originID, h.receipt.ReplyMode != channels.ReplyModeDirect)
	if err != nil {
		return fmt.Errorf("send final COT answer: %w", err)
	}
	if err := persistFinal(finalID); err != nil {
		return fmt.Errorf("persist final COT answer receipt: %w", err)
	}
	event, err := newCOTEvent("RUN_FINISHED", map[string]any{"threadId": h.receipt.COTID, "runId": h.runID, "status": "done"}, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := h.api.Append(ctx, h.receipt, []*larkim.MessageCot{event}); err != nil {
		return fmt.Errorf("finish COT: %w", err)
	}
	return nil
}

func (h *cotProgressHandle) Fail(ctx context.Context, progress channels.Progress) error {
	message := strings.TrimSpace(progress.Text)
	if message == "" {
		message = "Zotigo could not complete this task."
	}
	event, err := newCOTEvent("RUN_ERROR", map[string]any{"message": message, "code": "ZOTIGO_TASK_FAILED"}, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := h.api.Append(ctx, h.receipt, []*larkim.MessageCot{event}); err != nil {
		return err
	}
	return h.api.Complete(ctx, h.receipt, "error")
}

func (h *cotProgressHandle) Recover(ctx context.Context, completed bool) error {
	if completed {
		event, err := newCOTEvent("RUN_FINISHED", map[string]any{"threadId": h.receipt.COTID, "runId": h.runID, "status": "done"}, time.Now().UTC())
		if err != nil {
			return err
		}
		return h.api.Append(ctx, h.receipt, []*larkim.MessageCot{event})
	}
	return h.Fail(ctx, channels.Progress{State: "failed", Text: "Zotigo restarted before this task could finish. Open the bound session to inspect its state."})
}

func renderCOTEvents(threadID, runID string, events []channels.PublicExecutionEvent) ([]*larkim.MessageCot, error) {
	result := make([]*larkim.MessageCot, 0, len(events)*2)
	for _, event := range events {
		at := event.Timestamp
		if at.IsZero() {
			at = time.Now().UTC()
		}
		var eventType string
		var content map[string]any
		switch event.Type {
		case channels.ExecutionRunStarted:
			eventType = "RUN_STARTED"
			content = map[string]any{"threadId": threadID, "runId": runID}
		case channels.ExecutionToolStarted:
			if event.Tool == nil {
				continue
			}
			start, err := newCOTEvent("TOOL_CALL_START", map[string]any{"toolCallId": event.Tool.CallID, "toolCallName": event.Tool.Kind, "title": event.Tool.DisplayName, "icon": cotToolIcon(event.Tool.Kind)}, at)
			if err != nil {
				return nil, err
			}
			end, err := newCOTEvent("TOOL_CALL_END", map[string]any{"toolCallId": event.Tool.CallID}, at)
			if err != nil {
				return nil, err
			}
			result = append(result, start, end)
			continue
		case channels.ExecutionToolFinished:
			if event.Tool == nil {
				continue
			}
			eventType = "TOOL_CALL_RESULT"
			resultText := "Completed"
			if event.Tool.Status == "failed" {
				resultText = "Failed"
			}
			content = map[string]any{"messageId": event.ID, "toolCallId": event.Tool.CallID, "content": map[string]any{"type": "text", "text": resultText}, "role": "tool"}
		case channels.ExecutionApproval:
			stepID := event.CorrelationID
			if stepID == "" {
				stepID = event.ID
			}
			eventType = "STEP_STARTED"
			content = map[string]any{"stepName": "Waiting for approval in Zotigo", "stepId": stepID}
		case channels.ExecutionApprovalDone:
			stepID := event.CorrelationID
			if stepID == "" {
				stepID = event.ID
			}
			eventType = "STEP_FINISHED"
			content = map[string]any{"stepName": "Waiting for approval in Zotigo", "stepId": stepID}
		default:
			continue
		}
		cotEvent, err := newCOTEvent(eventType, content, at)
		if err != nil {
			return nil, err
		}
		result = append(result, cotEvent)
	}
	return result, nil
}

func newCOTEvent(eventType string, content any, at time.Time) (*larkim.MessageCot, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 4096 {
		return nil, fmt.Errorf("COT event %s exceeds 4096 bytes", eventType)
	}
	return larkim.NewMessageCotBuilder().EventType(eventType).Content(string(encoded)).Timestamp(strconv.FormatInt(at.UnixMilli(), 10)).Build(), nil
}

func cotToolIcon(name string) string {
	switch name {
	case "shell", "exec_command":
		return "bash"
	case "read_file", "grep", "glob":
		return "read"
	case "read_messages", "channel_read_messages":
		return "read"
	case "write_file", "edit":
		return "write"
	case "web_search", "web_fetch":
		return "search"
	default:
		return "default"
	}
}

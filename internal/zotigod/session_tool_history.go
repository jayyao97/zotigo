package zotigod

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/session"
)

// Budget serialized rows, not just text: escaping can expand one byte to six.
const toolHistoryPageBytes = 32 * 1024

type toolHistoryAttachment struct {
	Type      string `json:"type"`
	FileID    string `json:"file_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

func projectHistoryItem(item session.DisplayItem) toolHistoryItem {
	row := toolHistoryItem{Sequence: item.Sequence, Type: item.Type, Timestamp: item.CreatedAt}
	if item.Turn != nil {
		row.TurnID = item.Turn.ID
	}
	for _, part := range item.Content {
		if part.IsTool() {
			continue
		}
		row.Text += part.Text
		if part.Image != nil || part.Type != "text" && part.Text == "" {
			attachment := toolHistoryAttachment{Type: part.Type}
			if part.Image != nil {
				attachment.FileID = part.Image.FileID
				attachment.MediaType = part.Image.MediaType
			}
			// Do not embed image bytes or URLs in a text history response.
			row.Attachments = append(row.Attachments, attachment)
		}
	}
	return row
}

func historyItemBytes(row toolHistoryItem) int {
	encoded, _ := json.Marshal(row)
	return len(encoded) + 1 // JSON array separator
}

func historyTextChunk(row toolHistoryItem, budget int) (toolHistoryItem, error) {
	text := row.Text
	row.Truncated = true
	// Binary search byte boundaries; boundedToolText backs off partial UTF-8.
	low, high := 0, len(text)
	for low < high {
		middle := low + (high-low+1)/2
		row.Text = boundedToolText(text, middle)
		next := row.TextOffset + len(row.Text)
		row.NextTextOffset = &next
		if historyItemBytes(row) <= budget {
			low = middle
		} else {
			high = middle - 1
		}
	}
	row.Text = boundedToolText(text, low)
	next := row.TextOffset + len(row.Text)
	row.NextTextOffset = &next
	if row.Text == "" || historyItemBytes(row) > budget {
		return row, errors.New("message attachment metadata exceeds history response budget")
	}
	return row, nil
}

func projectToolHistory(page *session.DisplayPage, forward bool, textOffset int) ([]toolHistoryItem, error) {
	result := make([]toolHistoryItem, 0, len(page.Items))
	budget := toolHistoryPageBytes
	for step := range page.Items {
		index := step
		if !forward {
			index = len(page.Items) - 1 - step
		}
		row := projectHistoryItem(page.Items[index])
		if textOffset > len(row.Text) || textOffset < len(row.Text) && !utf8.RuneStart(row.Text[textOffset]) {
			return nil, errors.New("text_offset must be a UTF-8 byte boundary within the message")
		}
		row.TextOffset = textOffset
		row.Text = row.Text[textOffset:]
		if historyItemBytes(row) > budget {
			if len(result) != 0 {
				break // The cursor must stop before this unconsumed message.
			}
			var err error
			row, err = historyTextChunk(row, budget)
			if err != nil {
				return nil, err
			}
		}
		budget -= historyItemBytes(row)
		result = append(result, row)
	}
	if !forward {
		slices.Reverse(result)
	}
	if len(result) != 0 && len(result) < len(page.Items) {
		if forward {
			page.NextCursor = strconv.FormatUint(result[len(result)-1].Sequence, 10)
		} else {
			page.PrevCursor = strconv.FormatUint(result[0].Sequence, 10)
		}
		page.HasMore = true
	}
	return result, nil
}

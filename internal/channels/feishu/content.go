package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	channeltypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
)

var (
	markdownAtPattern    = regexp.MustCompile(`<at\s+user_id\s*=\s*"([^"]+)">(.*?)</at>`)
	markdownImagePattern = regexp.MustCompile(`!\[[^]]*\]\(([^)]+)\)`)
)

const (
	maxInboundImages     = 5
	maxInboundImageBytes = 5 << 20
)

type inboundContent struct {
	text         string
	imageKeys    []string
	mentionedBot bool
}

func parseInboundContent(messageType, raw string, bot channeltypes.BotIdentity) (inboundContent, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return inboundContent{}, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return inboundContent{}, errors.New("content must be an object")
	}
	if messageType == "image" {
		key, _ := root["image_key"].(string)
		if strings.TrimSpace(key) == "" {
			return inboundContent{}, errors.New("image_key is required")
		}
		return inboundContent{imageKeys: []string{strings.TrimSpace(key)}}, nil
	}
	document, err := selectPostDocument(root)
	if err != nil {
		return inboundContent{}, err
	}
	var lines []string
	if title, _ := document["title"].(string); strings.TrimSpace(title) != "" {
		lines = append(lines, strings.TrimSpace(title))
	}
	rows, _ := document["content_v2"].([]any)
	if len(rows) == 0 {
		rows, _ = document["content"].([]any)
	}
	if len(rows) == 0 {
		return inboundContent{}, errors.New("post content must be a non-empty array")
	}
	parsed := inboundContent{}
	for _, rawRow := range rows {
		row, ok := rawRow.([]any)
		if !ok {
			continue
		}
		var line strings.Builder
		for _, rawNode := range row {
			node, ok := rawNode.(map[string]any)
			if !ok {
				continue
			}
			switch tag, _ := node["tag"].(string); tag {
			case "text", "a":
				value, _ := node["text"].(string)
				line.WriteString(value)
			case "md":
				value, _ := node["text"].(string)
				md := parseMarkdownContent(value, bot)
				line.WriteString(md.text)
				parsed.imageKeys = append(parsed.imageKeys, md.imageKeys...)
				parsed.mentionedBot = parsed.mentionedBot || md.mentionedBot
			case "at":
				userID, _ := node["user_id"].(string)
				if userID == bot.OpenID || (bot.UserID != "" && userID == bot.UserID) {
					parsed.mentionedBot = true
					continue
				}
				name, _ := node["user_name"].(string)
				if strings.TrimSpace(name) != "" {
					line.WriteString("@" + strings.TrimSpace(name))
				}
			case "img":
				key, _ := node["image_key"].(string)
				if strings.TrimSpace(key) != "" {
					parsed.imageKeys = append(parsed.imageKeys, strings.TrimSpace(key))
				}
			}
		}
		if value := strings.TrimSpace(line.String()); value != "" {
			lines = append(lines, value)
		}
	}
	if len(parsed.imageKeys) > maxInboundImages {
		return inboundContent{}, fmt.Errorf("post contains more than %d images", maxInboundImages)
	}
	parsed.text = strings.Join(lines, "\n")
	return parsed, nil
}

func selectPostDocument(root map[string]any) (map[string]any, error) {
	if _, ok := root["content_v2"]; ok {
		return root, nil
	}
	if _, ok := root["content"]; ok {
		return root, nil
	}
	for _, locale := range []string{"zh_cn", "en_us", "ja_jp"} {
		if document, ok := root[locale].(map[string]any); ok {
			return document, nil
		}
	}
	keys := make([]string, 0, len(root))
	for key := range root {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if document, ok := root[key].(map[string]any); ok {
			return document, nil
		}
	}
	return nil, errors.New("post document is missing")
}

func parseMarkdownContent(text string, bot channeltypes.BotIdentity) inboundContent {
	parsed := inboundContent{}
	parts := strings.Split(text, "```")
	for index, part := range parts {
		insidePairedFence := index%2 == 1 && (len(parts)%2 != 0 || index != len(parts)-1)
		if insidePairedFence {
			continue
		}
		part = markdownAtPattern.ReplaceAllStringFunc(part, func(match string) string {
			groups := markdownAtPattern.FindStringSubmatch(match)
			if len(groups) != 3 {
				return match
			}
			if groups[1] == bot.OpenID || (bot.UserID != "" && groups[1] == bot.UserID) {
				parsed.mentionedBot = true
				return ""
			}
			if strings.TrimSpace(groups[2]) != "" {
				return "@" + strings.TrimSpace(groups[2])
			}
			return "@" + groups[1]
		})
		for _, groups := range markdownImagePattern.FindAllStringSubmatch(part, -1) {
			if len(groups) == 2 && strings.TrimSpace(groups[1]) != "" {
				parsed.imageKeys = append(parsed.imageKeys, strings.TrimSpace(groups[1]))
			}
		}
		parts[index] = markdownImagePattern.ReplaceAllString(part, "")
	}
	parsed.text = strings.Join(parts, "```")
	return parsed
}

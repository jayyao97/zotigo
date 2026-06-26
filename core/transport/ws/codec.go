package ws

import (
	"fmt"

	"github.com/bytedance/sonic"
)

func Encode(msg Message) ([]byte, error) {
	if msg.Type == "" {
		return nil, fmt.Errorf("websocket message type is required")
	}
	return sonic.Marshal(msg)
}

func Decode(data []byte) (Message, error) {
	var msg Message
	if err := sonic.Unmarshal(data, &msg); err != nil {
		return Message{}, err
	}
	if msg.Type == "" {
		return Message{}, fmt.Errorf("websocket message type is required")
	}
	return msg, nil
}

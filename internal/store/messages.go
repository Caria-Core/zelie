package store

import (
	"encoding/json"

	"github.com/Caria-Core/zelie/internal/msg"
)

// A message is kept in two columns: its English text, which is what rows
// written before messages had codes hold, and the message as JSON.
func msgColumns(m *msg.Msg) (text, js string) {
	if m == nil {
		return "", ""
	}
	b, _ := json.Marshal(m)
	return m.Text, string(b)
}

func readMsg(text, js string) *msg.Msg {
	var m msg.Msg
	if js != "" && json.Unmarshal([]byte(js), &m) == nil && m.Code != "" {
		return &m
	}
	if text == "" {
		return nil
	}
	m = msg.Other.With("detail", text)
	return &m
}

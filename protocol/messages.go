package protocol

import "encoding/json"

// Envelope wraps all WebSocket messages with a MsgType for routing.
type Envelope struct {
	MsgType string          `json:"MsgType"`
	Data    json.RawMessage `json:"Data,omitempty"`
}

// AuthMsg sent by client on connect.
type AuthMsg struct {
	MsgType  string `json:"MsgType"`
	Token    string `json:"Token"`
	ClientID string `json:"ClientID"`
}

// AuthResult sent by server after auth.
type AuthResult struct {
	MsgType string `json:"MsgType"`
	Reason  string `json:"Reason,omitempty"`
}

// SubscribeMsg sent by client after auth.
type SubscribeMsg struct {
	MsgType  string   `json:"MsgType"`
	Patterns []string `json:"Patterns"`
}

// EventMsg sent by server to client when a webhook arrives.
type EventMsg struct {
	MsgType        string            `json:"MsgType"`
	DeliveryID     string            `json:"DeliveryID"`
	GitHubEvent    string            `json:"GitHubEvent"`
	Provider       string            `json:"Provider,omitempty"`
	StableEvent    string            `json:"StableEvent,omitempty"`
	ExternalEvent  string            `json:"ExternalEvent,omitempty"`
	ExternalAction string            `json:"ExternalAction,omitempty"`
	Repo           string            `json:"Repo"`
	Ref            string            `json:"Ref,omitempty"`
	Action         string            `json:"Action,omitempty"`
	Sender         string            `json:"Sender,omitempty"`
	Message        string            `json:"Message,omitempty"`
	CloneURL       string            `json:"CloneURL"`
	PRNumber       int               `json:"PRNumber,omitempty"`
	Headers        map[string]string `json:"Headers,omitempty"`
	Metadata       map[string]string `json:"Metadata,omitempty"`
	Payload        json.RawMessage   `json:"Payload"`
}

// StatusMsg sent by client after processing an event.
type StatusMsg struct {
	MsgType     string `json:"MsgType"`
	DeliveryID  string `json:"DeliveryID"`
	State       string `json:"State"`
	Description string `json:"Description,omitempty"`
}

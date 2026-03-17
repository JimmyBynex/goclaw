package session

import (
	"errors"
	"fmt"
	"strings"
)

type Scope string

const (
	ScopeDM     Scope = "dm"
	ScopeGroup  Scope = "group"
	ScopeGlobal Scope = "global"
)

type SessionKey struct {
	ChannelID string `json:"channel_id"`
	AccountID string `json:"account_id"`
	Scope     Scope  `json:"scope"`
	PeerID    string `json:"peer_id"`
	AgentID   string `json:"agent_id"`
}

func (k SessionKey) String() string {
	return fmt.Sprintf("%s__%s__%s__%s__%s", k.ChannelID, k.AccountID, k.Scope, k.PeerID, k.AgentID)
}

func Parse(s string) (SessionKey, error) {
	var k SessionKey
	parts := strings.Split(s, "__")
	if len(parts) != 5 {
		return k, errors.New("invalid session key")
	}
	k.ChannelID = parts[0]
	k.AccountID = parts[1]
	k.Scope = Scope(parts[2])
	k.PeerID = parts[3]
	k.AgentID = parts[4]
	return k, nil

}

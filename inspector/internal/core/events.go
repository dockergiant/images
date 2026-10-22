package core

import "time"

type VarnishReq struct {
	VXID       string        `json:"vxid"`
	BereqVXID  string        `json:"bereqVxid,omitempty"`
	Method     string        `json:"method"`
	URL        string        `json:"url"`
	Host       string        `json:"host"`
	Status     int           `json:"status"`
	Handling   string        `json:"handling"`
	Start      time.Time     `json:"start"`
	TTFB       time.Duration `json:"ttfb"`
	Duration   time.Duration `json:"duration"`
	BereqStart time.Time     `json:"bereqStart,omitempty"`
	BereqEnd   time.Time     `json:"bereqEnd,omitempty"`
	Tags       []string      `json:"tags,omitempty"`
	TTL        string        `json:"ttl,omitempty"`
	Grace      bool          `json:"grace,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Raw        []string      `json:"raw,omitempty"`
}

type RedisCmd struct {
	Time      time.Time `json:"time"`
	DB        int       `json:"db"`
	Addr      string    `json:"addr"`
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	Cmd       string    `json:"cmd"`
	Args      []string  `json:"args"`
	PID       int       `json:"pid,omitempty"`
	Container string    `json:"container,omitempty"`
	Folded    int       `json:"folded,omitempty"`
}

type SearchQuery struct {
	Time     time.Time     `json:"time"`
	Start    time.Time     `json:"start"`
	Took     time.Duration `json:"took"`
	Index    string        `json:"index"`
	Label    string        `json:"label"`
	Phase    string        `json:"phase"`
	Summary  string        `json:"summary"`
	Body     string        `json:"body,omitempty"`
	OpaqueID string        `json:"opaqueId,omitempty"`
	Source   string        `json:"source"`
}

type MQPub struct {
	Time       time.Time `json:"time"`
	Exchange   string    `json:"exchange"`
	RoutingKey string    `json:"routingKey"`
	Queues     []string  `json:"queues"`
	Size       int       `json:"size"`
	ConnIP     string    `json:"connIp,omitempty"`
	ConnPort   int       `json:"connPort,omitempty"`
	PID        int       `json:"pid,omitempty"`
	Source     string    `json:"source"`
	Count      int       `json:"count"`
}

type NginxAccess struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	URL    string    `json:"url"`
	Status int       `json:"status"`
}

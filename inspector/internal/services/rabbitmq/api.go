package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrAuth = errors.New("rabbitmq rejected the credentials")

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rabbitmq management API returned %d: %s", e.Code, e.Body)
}

type Rate struct {
	Rate *float64 `json:"rate,omitempty"`
}

type MsgStats struct {
	Publish          *int64 `json:"publish,omitempty"`
	PublishDetails   *Rate  `json:"publish_details,omitempty"`
	Deliver          *int64 `json:"deliver,omitempty"`
	DeliverGet       *int64 `json:"deliver_get,omitempty"`
	Ack              *int64 `json:"ack,omitempty"`
	Get              *int64 `json:"get,omitempty"`
	GetEmpty         *int64 `json:"get_empty,omitempty"`
	Redeliver        *int64 `json:"redeliver,omitempty"`
	DropUnroutable   *int64 `json:"drop_unroutable,omitempty"`
	ReturnUnroutable *int64 `json:"return_unroutable,omitempty"`
}

type Totals struct {
	Messages      *int64 `json:"messages,omitempty"`
	Ready         *int64 `json:"messages_ready,omitempty"`
	Unacknowledge *int64 `json:"messages_unacknowledged,omitempty"`
}

type ObjectTotals struct {
	Channels    *int64 `json:"channels,omitempty"`
	Consumers   *int64 `json:"consumers,omitempty"`
	Exchanges   *int64 `json:"exchanges,omitempty"`
	Queues      *int64 `json:"queues,omitempty"`
	Connections *int64 `json:"connections,omitempty"`
}

type Overview struct {
	ManagementVersion string        `json:"management_version,omitempty"`
	RabbitMQVersion   string        `json:"rabbitmq_version,omitempty"`
	ProductVersion    string        `json:"product_version,omitempty"`
	ClusterName       string        `json:"cluster_name,omitempty"`
	MessageStats      *MsgStats     `json:"message_stats,omitempty"`
	QueueTotals       *Totals       `json:"queue_totals,omitempty"`
	ObjectTotals      *ObjectTotals `json:"object_totals,omitempty"`
}

func (o *Overview) Version() string {
	if o.RabbitMQVersion != "" {
		return o.RabbitMQVersion
	}
	return o.ProductVersion
}

type Queue struct {
	Name                 string         `json:"name"`
	VHost                string         `json:"vhost,omitempty"`
	State                string         `json:"state,omitempty"`
	Type                 string         `json:"type,omitempty"`
	Durable              *bool          `json:"durable,omitempty"`
	AutoDelete           *bool          `json:"auto_delete,omitempty"`
	Exclusive            *bool          `json:"exclusive,omitempty"`
	Consumers            *int64         `json:"consumers,omitempty"`
	Messages             *int64         `json:"messages,omitempty"`
	Ready                *int64         `json:"messages_ready,omitempty"`
	Unacknowledged       *int64         `json:"messages_unacknowledged,omitempty"`
	MessageBytes         *int64         `json:"message_bytes,omitempty"`
	Memory               *int64         `json:"memory,omitempty"`
	HeadMessageTimestamp *int64         `json:"head_message_timestamp,omitempty"`
	MessageStats         *MsgStats      `json:"message_stats,omitempty"`
	Arguments            map[string]any `json:"arguments,omitempty"`
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (q *Queue) ReadyCount() int64    { return val(q.Ready) }
func (q *Queue) UnackedCount() int64  { return val(q.Unacknowledged) }
func (q *Queue) ConsumerCount() int64 { return val(q.Consumers) }
func (q *Queue) Stat(f func(*MsgStats) *int64) int64 {
	if q.MessageStats == nil {
		return 0
	}
	return val(f(q.MessageStats))
}

type ChannelDetails struct {
	ConnectionName string `json:"connection_name,omitempty"`
	Name           string `json:"name,omitempty"`
	Number         int    `json:"number,omitempty"`
	PeerHost       string `json:"peer_host,omitempty"`
	PeerPort       int    `json:"peer_port,omitempty"`
	User           string `json:"user,omitempty"`
}

type QueueRef struct {
	Name  string `json:"name"`
	VHost string `json:"vhost,omitempty"`
}

type Consumer struct {
	Queue          QueueRef       `json:"queue"`
	ConsumerTag    string         `json:"consumer_tag,omitempty"`
	PrefetchCount  *int64         `json:"prefetch_count,omitempty"`
	Active         *bool          `json:"active,omitempty"`
	AckRequired    *bool          `json:"ack_required,omitempty"`
	Exclusive      *bool          `json:"exclusive,omitempty"`
	ChannelDetails ChannelDetails `json:"channel_details"`
}

type ClientProperties struct {
	Product        string `json:"product,omitempty"`
	Platform       string `json:"platform,omitempty"`
	Version        string `json:"version,omitempty"`
	ConnectionName string `json:"connection_name,omitempty"`
}

type Connection struct {
	Name             string           `json:"name"`
	User             string           `json:"user,omitempty"`
	State            string           `json:"state,omitempty"`
	Channels         *int64           `json:"channels,omitempty"`
	PeerHost         string           `json:"peer_host,omitempty"`
	PeerPort         int              `json:"peer_port,omitempty"`
	ConnectedAt      *int64           `json:"connected_at,omitempty"`
	ClientProperties ClientProperties `json:"client_properties"`
}

type Exchange struct {
	Name       string `json:"name"`
	VHost      string `json:"vhost,omitempty"`
	Type       string `json:"type,omitempty"`
	Durable    *bool  `json:"durable,omitempty"`
	AutoDelete *bool  `json:"auto_delete,omitempty"`
	Internal   *bool  `json:"internal,omitempty"`
}

type Binding struct {
	Source          string `json:"source"`
	Destination     string `json:"destination"`
	DestinationType string `json:"destination_type"`
	RoutingKey      string `json:"routing_key"`
	VHost           string `json:"vhost,omitempty"`
}

type VHost struct {
	Name    string `json:"name"`
	Tracing bool   `json:"tracing"`
}

type PeekedMessage struct {
	PayloadBytes    int            `json:"payload_bytes"`
	Redelivered     bool           `json:"redelivered"`
	Exchange        string         `json:"exchange"`
	RoutingKey      string         `json:"routing_key"`
	MessageCount    int            `json:"message_count"`
	Properties      map[string]any `json:"properties,omitempty"`
	Payload         string         `json:"payload"`
	PayloadEncoding string         `json:"payload_encoding"`
}

type Client struct {
	base  string
	user  string
	pass  string
	vhost string
	http  *http.Client
}

func NewClient(base, user, pass, vhost string) *Client {
	if vhost == "" {
		vhost = "/"
	}
	return &Client{
		base:  strings.TrimRight(base, "/"),
		user:  user,
		pass:  pass,
		vhost: vhost,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) VHostPath() string {
	return url.PathEscape(c.vhost)
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrAuth
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: truncate(string(data), 300)}
	}
	return data, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Overview(ctx context.Context) (*Overview, error) {
	var o Overview
	if err := c.get(ctx, "/api/overview", &o); err != nil {
		return nil, err
	}
	return &o, nil
}

const queueColumns = "name,vhost,state,type,durable,auto_delete,exclusive,consumers,messages,messages_ready,messages_unacknowledged,message_bytes,memory,head_message_timestamp,message_stats,arguments"

func ParseQueues(data []byte) (queues []Queue, pageCount int, err error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		return queues, 1, json.Unmarshal(data, &queues)
	}
	var env struct {
		Items     []Queue `json:"items"`
		PageCount int     `json:"page_count"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, err
	}
	return env.Items, max(env.PageCount, 1), nil
}

func (c *Client) Queues(ctx context.Context) ([]Queue, error) {
	var all []Queue
	for page := 1; page <= 20; page++ {
		path := fmt.Sprintf("/api/queues/%s?page=%d&page_size=500&columns=%s", c.VHostPath(), page, queueColumns)
		data, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		qs, pages, err := ParseQueues(data)
		if err != nil {
			return nil, err
		}
		all = append(all, qs...)
		if page >= pages {
			break
		}
	}
	return all, nil
}

func (c *Client) QueueDepths(ctx context.Context) ([]Queue, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/queues/"+c.VHostPath()+"?disable_stats=true&enable_queue_totals=true", nil)
	if err != nil {
		return nil, err
	}
	qs, _, err := ParseQueues(data)
	return qs, err
}

func (c *Client) Queue(ctx context.Context, name string) (*Queue, error) {
	var q Queue
	if err := c.get(ctx, "/api/queues/"+c.VHostPath()+"/"+url.PathEscape(name), &q); err != nil {
		return nil, err
	}
	return &q, nil
}

func (c *Client) Consumers(ctx context.Context) ([]Consumer, error) {
	var out []Consumer
	return out, c.get(ctx, "/api/consumers/"+c.VHostPath(), &out)
}

func (c *Client) Connections(ctx context.Context) ([]Connection, error) {
	var out []Connection
	return out, c.get(ctx, "/api/connections", &out)
}

func (c *Client) Exchanges(ctx context.Context) ([]Exchange, error) {
	var out []Exchange
	return out, c.get(ctx, "/api/exchanges/"+c.VHostPath(), &out)
}

func (c *Client) Bindings(ctx context.Context) ([]Binding, error) {
	var out []Binding
	return out, c.get(ctx, "/api/bindings/"+c.VHostPath(), &out)
}

func (c *Client) VHost(ctx context.Context) (*VHost, error) {
	var v VHost
	if err := c.get(ctx, "/api/vhosts/"+c.VHostPath(), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) SetTracing(ctx context.Context, on bool) error {
	_, err := c.do(ctx, http.MethodPut, "/api/vhosts/"+c.VHostPath(), map[string]bool{"tracing": on})
	return err
}

func (c *Client) Peek(ctx context.Context, queue string, count int) ([]PeekedMessage, error) {
	body := map[string]any{
		"count":    count,
		"ackmode":  "ack_requeue_true",
		"encoding": "auto",
		"truncate": 50000,
	}
	data, err := c.do(ctx, http.MethodPost, "/api/queues/"+c.VHostPath()+"/"+url.PathEscape(queue)+"/get", body)
	if err != nil {
		return nil, err
	}
	var out []PeekedMessage
	return out, json.Unmarshal(data, &out)
}

func (c *Client) Purge(ctx context.Context, queue string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/queues/"+c.VHostPath()+"/"+url.PathEscape(queue)+"/contents", nil)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..." + strconv.Itoa(len(s)-n) + " more"
}

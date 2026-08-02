package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type Envelope struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Read      bool      `json:"read"`
	Timestamp time.Time `json:"timestamp"`
}

type Mailbox struct {
	mu       sync.RWMutex
	MailFile string
}

func NewMailbox(mailFile string) *Mailbox {
	return &Mailbox{MailFile: mailFile}
}

func (m *Mailbox) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(m.MailFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mail directory: %w", err)
	}

	env := &Envelope{
		ID:        newID(),
		Sender:    sender,
		Recipient: recipient,
		Subject:   subject,
		Body:      body,
		Read:      false,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal mail envelope: %w", err)
	}

	f, err := os.OpenFile(m.MailFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open mail file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write mail entry: %w", err)
	}

	return env, nil
}

func (m *Mailbox) ReadInbox(recipient string) ([]*Envelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, err := os.Stat(m.MailFile); os.IsNotExist(err) {
		return nil, nil
	}

	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read mail file: %w", err)
	}

	var res []*Envelope

	rawLines := splitLines(string(data))
	for _, l := range rawLines {
		if len(l) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(l), &env); err == nil {
			if recipient == "" || env.Recipient == recipient || env.Recipient == "all" {
				res = append(res, &env)
			}
		}
	}

	return res, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

var idCounter atomic.Int64

func init() {
	idCounter.Store(time.Now().UnixMilli())
}

func newID() string {
	n := idCounter.Add(1)
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, n)
}

// RedisClient defines the Redis surface we consume.
type RedisClient interface {
	Publish(ctx context.Context, channel string, message interface{}) *redis.IntCmd
	Subscribe(ctx context.Context, channels ...string) Subscription
	Close() error
}

// Subscription is the consumer side of a Redis pub/sub channel.
type Subscription interface {
	Channel(opts ...redis.ChannelOption) <-chan *redis.Message
	Close() error
}

// MessageBrokerOption configures a MessageBroker.
type MessageBrokerOption func(*MessageBroker)

func WithRedis(client RedisClient, channelPrefix string) MessageBrokerOption {
	return func(b *MessageBroker) {
		b.redis = client
		b.channelPrefix = channelPrefix
	}
}

// MessageBroker wraps a local Mailbox with optional Redis pub/sub syncing.
type MessageBroker struct {
	mb            *Mailbox
	redis         RedisClient
	channelPrefix string
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func NewMessageBroker(mailbox *Mailbox, opts ...MessageBrokerOption) *MessageBroker {
	ctx, cancel := context.WithCancel(context.Background())
	b := &MessageBroker{
		mb:     mailbox,
		ctx:    ctx,
		cancel: cancel,
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.redis != nil {
		b.startSubscriber()
	}
	return b
}

func (b *MessageBroker) Close() error {
	b.cancel()
	b.wg.Wait()
	if b.redis != nil {
		return b.redis.Close()
	}
	return nil
}

func (b *MessageBroker) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	env, err := b.mb.SendMessage(sender, recipient, subject, body)
	if err != nil {
		return nil, err
	}
	if b.redis != nil {
		channel := b.channelPrefix + "." + recipient
		data, _ := json.Marshal(env)
		b.redis.Publish(b.ctx, channel, data)
	}
	return env, nil
}

func (b *MessageBroker) ReadInbox(recipient string) ([]*Envelope, error) {
	return b.mb.ReadInbox(recipient)
}

func (b *MessageBroker) startSubscriber() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		sub := b.redis.Subscribe(b.ctx, b.channelPrefix+".*")
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-b.ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var env Envelope
				if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
					continue
				}
				b.mb.mu.Lock()
				f, err := os.OpenFile(b.mb.MailFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
				if err != nil {
					b.mb.mu.Unlock()
					continue
				}
				f.Write(append([]byte(msg.Payload), '\n'))
				f.Close()
				b.mb.mu.Unlock()
			}
		}
	}()
}

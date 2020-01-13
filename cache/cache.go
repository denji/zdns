package cache

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/mpolden/zdns/dns/dnsutil"
)

// Backend is the interface for a cache backend. All write operations in a Cache are forwarded to a Backend.
type Backend interface {
	Set(key uint32, value Value)
	Evict(key uint32)
	Read() []Value
	Reset()
}

type defaultBackend struct{}

func (b *defaultBackend) Set(uint32, Value) {}
func (b *defaultBackend) Evict(uint32)      {}
func (b *defaultBackend) Read() []Value     { return nil }
func (b *defaultBackend) Reset()            {}

// Cache is a cache of DNS messages.
type Cache struct {
	client   *dnsutil.Client
	backend  Backend
	capacity int
	values   map[uint32]Value
	keys     []uint32
	mu       sync.RWMutex
	now      func() time.Time
	queue    chan func()
	wg       sync.WaitGroup
}

// Value wraps a DNS message stored in the cache.
type Value struct {
	Key       uint32
	CreatedAt time.Time
	msg       *dns.Msg
}

// Stats contains cache statistics.
type Stats struct {
	Size         int
	Capacity     int
	PendingTasks int
}

// Rcode returns the response code of the cached value v.
func (v *Value) Rcode() int { return v.msg.Rcode }

// Question returns the first question the cached value v.
func (v *Value) Question() string { return v.msg.Question[0].Name }

// Qtype returns the query type of the cached value v
func (v *Value) Qtype() uint16 { return v.msg.Question[0].Qtype }

// Answers returns the answers of the cached value v.
func (v *Value) Answers() []string { return dnsutil.Answers(v.msg) }

// TTL returns the time to live of the cached value v.
func (v *Value) TTL() time.Duration { return dnsutil.MinTTL(v.msg) }

// Pack returns a string representation of Value v.
func (v *Value) Pack() (string, error) {
	var sb strings.Builder
	sb.WriteString(strconv.FormatUint(uint64(v.Key), 10))
	sb.WriteString(" ")
	sb.WriteString(strconv.FormatInt(v.CreatedAt.Unix(), 10))
	sb.WriteString(" ")
	data, err := v.msg.Pack()
	if err != nil {
		return "", err
	}
	sb.WriteString(hex.EncodeToString(data))
	return sb.String(), nil
}

// Unpack converts a string value into a Value type.
func Unpack(value string) (Value, error) {
	fields := strings.Fields(value)
	if len(fields) < 3 {
		return Value{}, fmt.Errorf("invalid number of fields: %q", value)
	}
	key, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return Value{}, err
	}
	secs, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Value{}, err
	}
	data, err := hex.DecodeString(fields[2])
	if err != nil {
		return Value{}, err
	}
	msg := &dns.Msg{}
	if err := msg.Unpack(data); err != nil {
		return Value{}, err
	}
	return Value{
		Key:       uint32(key),
		CreatedAt: time.Unix(secs, 0),
		msg:       msg,
	}, nil
}

// New creates a new cache of given capacity.
//
// If client is non-nil, the cache will prefetch expired entries in an effort to serve results faster.
//
// If backend is non-nil:
//
// - All cache write operations will be forward to the backend.
// - The backed will be used to pre-populate the cache.
func New(capacity int, client *dnsutil.Client) *Cache {
	return NewWithBackend(capacity, client, &defaultBackend{})
}

// NewWithBackend creates a new cache that forwards entries to backend.
func NewWithBackend(capacity int, client *dnsutil.Client, backend Backend) *Cache {
	return newCache(capacity, client, backend, time.Now)
}

func newCache(capacity int, client *dnsutil.Client, backend Backend, now func() time.Time) *Cache {
	if capacity < 0 {
		capacity = 0
	}
	c := &Cache{
		client:   client,
		backend:  &defaultBackend{},
		now:      now,
		capacity: capacity,
		values:   make(map[uint32]Value, capacity),
		queue:    make(chan func(), 1024),
	}
	c.load(backend)
	go c.readQueue()
	return c
}

// NewKey creates a new cache key for the DNS name, qtype and qclass
func NewKey(name string, qtype, qclass uint16) uint32 {
	h := fnv.New32a()
	h.Write([]byte(name))
	binary.Write(h, binary.BigEndian, qtype)
	binary.Write(h, binary.BigEndian, qclass)
	return h.Sum32()
}

func (c *Cache) load(backend Backend) {
	if c.capacity == 0 {
		backend.Reset()
		return
	}
	values := backend.Read()
	n := 0
	if c.capacity < len(values) {
		n = c.capacity
	}
	// Add the last n values from backend
	for _, v := range values[n:] {
		c.setValue(v)
	}
	if c.capacity < len(values) {
		// Remove older entries from backend
		for _, v := range values[:n] {
			backend.Evict(v.Key)
		}
	}
	c.backend = backend
}

// Close consumes any outstanding cache operations.
func (c *Cache) Close() error {
	c.wg.Wait()
	return nil
}

// Get returns the DNS message associated with key.
func (c *Cache) Get(key uint32) (*dns.Msg, bool) {
	v, ok := c.getValue(key)
	if !ok {
		return nil, false
	}
	return v.msg, true
}

func (c *Cache) getValue(key uint32) (*Value, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.values[key]
	if !ok {
		return nil, false
	}
	if c.isExpired(&v) {
		if !c.prefetch() {
			c.enqueue(func() { c.evictWithLock(key) })
			return nil, false
		}
		c.enqueue(func() { c.refresh(key, v.msg) })
	}
	return &v, true
}

// List returns the n most recent values in cache c.
func (c *Cache) List(n int) []Value {
	values := make([]Value, 0, n)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := len(c.keys) - 1; i >= 0; i-- {
		if len(values) == n {
			break
		}
		v := c.values[c.keys[i]]
		values = append(values, v)
	}
	return values
}

// Set associates key with the DNS message msg.
//
// If prefetching is disabled, the message will be evicted from the cache according to its TTL.
//
// If prefetching is enabled, the message will never be evicted, but it will be refreshed when the TTL passes.
//
// Setting a new key in a cache that has reached its capacity will evict values in a FIFO order.
func (c *Cache) Set(key uint32, msg *dns.Msg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(key, msg)
}

// Stats returns cache statistics.
func (c *Cache) Stats() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Stats{
		Capacity:     c.capacity,
		Size:         len(c.values),
		PendingTasks: len(c.queue),
	}
}

func (c *Cache) set(key uint32, msg *dns.Msg) bool {
	return c.setValue(Value{Key: key, CreatedAt: c.now(), msg: msg})
}

func (c *Cache) setValue(value Value) bool {
	if c.capacity == 0 || !canCache(value.msg) {
		return false
	}
	if len(c.values) == c.capacity && c.capacity > 0 {
		evict := c.keys[0]
		delete(c.values, evict)
		c.keys = c.keys[1:]
		c.backend.Evict(evict)
	}
	c.values[value.Key] = value
	c.appendKey(value.Key)
	c.backend.Set(value.Key, value)
	return true
}

// Reset removes all values contained in cache c.
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = make(map[uint32]Value)
	c.keys = nil
	c.backend.Reset()
}

func (c *Cache) prefetch() bool { return c.client != nil }

func (c *Cache) refresh(key uint32, old *dns.Msg) {
	q := old.Question[0]
	msg := dns.Msg{}
	msg.SetQuestion(q.Name, q.Qtype)
	r, err := c.client.Exchange(&msg)
	if err != nil {
		return // Retry on next request
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set(key, r) {
		c.evict(key)
	}
}

func (c *Cache) evictWithLock(key uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evict(key)
}

func (c *Cache) evict(key uint32) {
	delete(c.values, key)
	c.removeKey(key)
	c.backend.Evict(key)
}

func (c *Cache) appendKey(key uint32) {
	c.removeKey(key)
	c.keys = append(c.keys, key)
}

func (c *Cache) removeKey(key uint32) {
	var keys []uint32
	for _, k := range c.keys {
		if k == key {
			continue
		}
		keys = append(keys, k)
	}
	c.keys = keys
}

func (c *Cache) isExpired(v *Value) bool {
	expiresAt := v.CreatedAt.Add(dnsutil.MinTTL(v.msg))
	return c.now().After(expiresAt)
}

func (c *Cache) enqueue(op func()) {
	c.wg.Add(1)
	c.queue <- op
}

func (c *Cache) readQueue() {
	for op := range c.queue {
		op()
		c.wg.Done()
	}
}

func canCache(msg *dns.Msg) bool {
	if dnsutil.MinTTL(msg) == 0 {
		return false
	}
	return msg.Rcode == dns.RcodeSuccess || msg.Rcode == dns.RcodeNameError
}

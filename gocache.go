package gocache

import (
	"bufio"
	"encoding/gob"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

const (
	// NoMaxSize means that the cache has no maximum number of entries in the cache
	// Setting MaxSize to this value also means there will be no eviction
	NoMaxSize = 0

	// DefaultMaxSize is the max size set if no max size is specified
	DefaultMaxSize = 1000

	NoExpiration = -1
)

var (
	ErrKeyDoesNotExist    = errors.New("key does not exist")
	ErrKeyHasNoExpiration = errors.New("key has no expiration")
)

type Cache struct {
	// MaxSize is the maximum amount of entries that can be in the cache at any given time
	MaxSize int

	// EvictionPolicy is the eviction policy
	EvictionPolicy EvictionPolicy

	entries map[string]*Entry
	mutex   sync.RWMutex

	head *Entry
	tail *Entry
}

// NewCache creates a new Cache
func NewCache() *Cache {
	return &Cache{
		MaxSize:        DefaultMaxSize,
		EvictionPolicy: FirstInFirstOut,
		entries:        make(map[string]*Entry),
		mutex:          sync.RWMutex{},
	}
}

// WithMaxSize sets the maximum amount of entries that can be in the cache at any given time
// A MaxSize of 0 or less means infinite
func (cache *Cache) WithMaxSize(maxSize int) *Cache {
	if maxSize < 0 {
		maxSize = NoMaxSize
	}
	cache.MaxSize = maxSize
	return cache
}

// WithEvictionPolicy sets eviction algorithm.
// Defaults to FirstInFirstOut (FIFO)
func (cache *Cache) WithEvictionPolicy(policy EvictionPolicy) *Cache {
	cache.EvictionPolicy = policy
	return cache
}

// Set creates or updates a key with a given value
func (cache *Cache) Set(key string, value interface{}) {
	cache.SetWithTTL(key, value, NoExpiration)
}

// SetWithTTL creates or updates a key with a given value and sets an expiration time (-1 is no expiration)
func (cache *Cache) SetWithTTL(key string, value interface{}, ttl time.Duration) {
	cache.mutex.Lock()
	entry, ok := cache.entries[key]
	if !ok {
		// A negative TTL that isn't -1 (NoExpiration) is an entry that will expire instantly,
		// so might as well just not create it in the first place    XXX: is this even necessary?
		if ttl != NoExpiration && ttl < 0 {
			cache.mutex.Unlock()
			return
		}
		// Cache entry doesn't exist, so we have to create a new one
		entry = &Entry{
			Key:               key,
			Value:             value,
			RelevantTimestamp: time.Now(),
			previous:          cache.head,
		}
		if cache.head == nil {
			cache.tail = entry
		} else {
			cache.head.next = entry
		}
		cache.head = entry
		cache.entries[key] = entry
	} else {
		entry.Value = value
		// Because we just updated the entry, we need to move it back to HEAD
		cache.moveExistingEntryToHead(entry)
	}
	if ttl != NoExpiration {
		entry.Expiration = time.Now().Add(ttl).UnixNano()
	} else {
		entry.Expiration = NoExpiration
	}
	// If the cache doesn't have a MaxSize, then there's no point checking if we need to evict
	// an entry, so we'll just return now
	if cache.MaxSize == NoMaxSize {
		cache.mutex.Unlock()
		return
	}
	if len(cache.entries) > cache.MaxSize {
		cache.evict()
	}
	cache.mutex.Unlock()
}

// Get retrieves an entry using the key passed as parameter
// If there is no such entry, the value returned will be nil and the boolean will be false
// If there is an entry, the value returned will be the value cached and the boolean will be true
func (cache *Cache) Get(key string) (interface{}, bool) {
	cache.mutex.RLock()
	entry, ok := cache.entries[key]
	cache.mutex.RUnlock()
	if !ok || entry.Expired() {
		return nil, false
	}
	if cache.EvictionPolicy == LeastRecentlyUsed {
		entry.Accessed()
		if cache.head == entry {
			return entry.Value, true
		}
		// Because the eviction policy is LRU, we need to move the entry back to HEAD
		// XXX: the following lock really hurts perf. Perhaps we should create a mutex specifically for head/tail?
		cache.mutex.Lock()
		cache.moveExistingEntryToHead(entry)
		cache.mutex.Unlock()
	}
	return entry.Value, true
}

// GetAll retrieves multiple entries using the keys passed as parameter
// All keys are returned in the map, regardless of whether they exist or not,
// however, entries that do not exist in the cache will return nil, meaning that
// there is no way of determining whether a key genuinely has the value nil, or
// whether it doesn't exist in the cache using only this function
func (cache *Cache) GetAll(keys []string) map[string]interface{} {
	entries := make(map[string]interface{})
	for _, key := range keys {
		entries[key], _ = cache.Get(key)
	}
	return entries
}

// Delete removes a key from the cache
// Returns false if the key did not exist.
func (cache *Cache) Delete(key string) bool {
	cache.mutex.Lock()
	entry, ok := cache.entries[key]
	if ok {
		cache.removeExistingEntryReferences(entry)
		delete(cache.entries, key)
	}
	cache.mutex.Unlock()
	return ok
}

// DeleteAll deletes multiple entries based on the keys passed as parameter
//
// Returns the number of keys deleted
func (cache *Cache) DeleteAll(keys []string) int {
	numberOfKeysDeleted := 0
	cache.mutex.Lock()
	for _, key := range keys {
		entry, ok := cache.entries[key]
		if ok {
			cache.removeExistingEntryReferences(entry)
			delete(cache.entries, key)
			numberOfKeysDeleted++
		}
	}
	cache.mutex.Unlock()
	return numberOfKeysDeleted
}

// Count returns the total amount of entries in the cache, regardless of whether they're expired or not
func (cache *Cache) Count() int {
	cache.mutex.RLock()
	count := len(cache.entries)
	cache.mutex.RUnlock()
	return count
}

// Clear deletes all entries from the cache
func (cache *Cache) Clear() {
	cache.mutex.Lock()
	cache.entries = make(map[string]*Entry)
	cache.head = nil
	cache.tail = nil
	cache.mutex.Unlock()
}

// TTL returns the time until the cache entry specified by the key passed as parameter
// will be deleted.
//
// Returns -1 if the key has no expiration
// Returns -2 if the key does not exist
func (cache *Cache) TTL(key string) (time.Duration, error) {
	cache.mutex.RLock()
	entry, ok := cache.entries[key]
	cache.mutex.RUnlock()
	if !ok {
		return 0, ErrKeyDoesNotExist
	}
	if entry.Expiration == NoExpiration {
		return 0, ErrKeyHasNoExpiration
	}
	timeUntilExpiration := time.Until(time.Unix(0, entry.Expiration))
	if timeUntilExpiration < 0 {
		// The key has already expired but hasn't been deleted yet.
		// From the client's perspective, this means that the cache entry doesn't exist
		return 0, ErrKeyDoesNotExist
	}
	return timeUntilExpiration, nil
}

// Expire sets a key's expiration time
//
// A ttl of -1 means that the key will never expire
// A ttl of 0 means that the key will expire immediately
// If using LRU, note that this does not reset the position of the key
//
// Returns true if the cache key exists and has had its expiration time altered
func (cache *Cache) Expire(key string, ttl time.Duration) bool {
	cache.mutex.RLock()
	entry, ok := cache.entries[key]
	cache.mutex.RUnlock()
	if !ok || entry.Expired() {
		return false
	}
	if ttl != NoExpiration {
		entry.Expiration = time.Now().Add(ttl).UnixNano()
	} else {
		entry.Expiration = NoExpiration
	}
	return true
}

// SaveToFile stores the content of the cache to a file so that it can be read using
// the ReadFromFile function
func (cache *Cache) SaveToFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	encoder := gob.NewEncoder(writer)
	cache.mutex.RLock()
	err = encoder.Encode(cache.entries)
	cache.mutex.RUnlock()
	if err != nil {
		return err
	}
	return writer.Flush()
}

// ReadFromFile populates the cache using a file created using cache.SaveToFile(path)
//
// Note that if the number of entries retrieved from the file exceed the configured MaxSize,
// the extra entries will be automatically evicted according to the EvictionPolicy configured.
// This function returns the number of entries evicted, and because this function only reads
// from a file and does not modify it, you can safely retry this function after configuring
// the cache with the appropriate MaxSize, should you desire to.
func (cache *Cache) ReadFromFile(path string) (int, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	decoder := gob.NewDecoder(reader)
	cache.mutex.Lock()
	err = decoder.Decode(&cache.entries)
	if err != nil {
		return 0, err
	}
	// Because pointers don't get stored in the file, we need to relink everything from head to tail
	var entries []*Entry
	for _, v := range cache.entries {
		entries = append(entries, v)
	}
	// Sort the slice of entries from oldest to newest
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelevantTimestamp.Before(entries[j].RelevantTimestamp)
	})
	// Relink the nodes from tail to head
	var previous *Entry
	for i := range entries {
		current := entries[i]
		if previous == nil {
			cache.tail = current
			cache.head = current
		} else {
			previous.next = current
			current.previous = previous
			cache.head = current
		}
		previous = entries[i]
	}
	// If the cache doesn't have a MaxSize, then there's no point checking if we need to evict
	// an entry, so we'll just return now
	if cache.MaxSize == NoMaxSize {
		cache.mutex.Unlock()
		return 0, nil
	}
	// Evict until the total number of entries matches the cache's maximum size
	numberOfEvictions := 0
	for len(cache.entries) > cache.MaxSize {
		numberOfEvictions++
		cache.evict()
	}
	cache.mutex.Unlock()
	return numberOfEvictions, nil
}

// moveExistingEntryToHead replaces the current cache head for an existing entry
func (cache *Cache) moveExistingEntryToHead(entry *Entry) {
	if !(entry == cache.head && entry == cache.tail) {
		cache.removeExistingEntryReferences(entry)
	}
	if entry != cache.head {
		entry.previous = cache.head
		entry.next = nil
		cache.head.next = entry
		cache.head = entry
	}
}

// removeExistingEntryReferences modifies the next and previous reference of an existing entry and re-links
// the next and previous entry accordingly, as well as the cache head or/and the cache tail if necessary.
// Note that it does not remove the entry from the cache, only the references.
func (cache *Cache) removeExistingEntryReferences(entry *Entry) {
	if cache.tail == entry {
		cache.tail = cache.tail.next
	}
	if cache.head == entry {
		cache.head = entry.previous
	}
	if entry.previous != nil {
		entry.previous.next = entry.next
	}
	if entry.next != nil {
		entry.next.previous = entry.previous
	}
}

// evict removes the tail from the cache
func (cache *Cache) evict() {
	if cache.tail == nil || len(cache.entries) == 0 {
		return
	}
	if cache.tail != nil {
		delete(cache.entries, cache.tail.Key)
		cache.tail = cache.tail.next
		cache.tail.previous = nil
	}
}
